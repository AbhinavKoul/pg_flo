package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/nats-io/nats.go" // Use the standard NATS package
	"github.com/pgflo/pg_flo/pkg/pgflonats"
	"github.com/pgflo/pg_flo/pkg/routing"
	"github.com/pgflo/pg_flo/pkg/rules"
	"github.com/pgflo/pg_flo/pkg/sinks"
	"github.com/pgflo/pg_flo/pkg/utils"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Worker represents a worker that processes messages from NATS.
type Worker struct {
	natsClient     *pgflonats.NATSClient
	ruleEngine     *rules.RuleEngine
	router         *routing.Router
	sink           sinks.Sink
	group          string
	logger         utils.Logger
	batchSize      int
	buffer         []*utils.CDCMessage
	lastSavedState uint64
	flushInterval  time.Duration
	shutdownCh     chan struct{}
	wg             sync.WaitGroup
}

// Option is a function type that modifies Worker configuration
type Option func(*Worker)

// WithBatchSize sets the batch size for the worker
func WithBatchSize(size int) Option {
	return func(w *Worker) {
		w.batchSize = size
	}
}

func init() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "15:04:05.000"})
	zerolog.TimeFieldFormat = "2006-01-02T15:04:05.000Z07:00"
}

// NewWorker creates and returns a new Worker instance with the provided NATS client
func NewWorker(natsClient *pgflonats.NATSClient, ruleEngine *rules.RuleEngine, router *routing.Router, sink sinks.Sink, group string, opts ...Option) *Worker {
	logger := utils.NewZerologLogger(log.With().Str("component", "worker").Logger())

	w := &Worker{
		natsClient:     natsClient,
		ruleEngine:     ruleEngine,
		router:         router,
		sink:           sink,
		group:          group,
		logger:         logger,
		batchSize:      1000,
		buffer:         make([]*utils.CDCMessage, 0, 1000),
		lastSavedState: 0,
		flushInterval:  500 * time.Millisecond,
		shutdownCh:     make(chan struct{}),
	}

	for _, opt := range opts {
		opt(w)
	}
	w.buffer = make([]*utils.CDCMessage, 0, w.batchSize)

	return w
}

// Start begins the worker's message processing loop, setting up the NATS consumer and processing messages.
func (w *Worker) Start(ctx context.Context) error {
	stream := fmt.Sprintf("pgflo_%s_stream", w.group)
	subject := fmt.Sprintf("pgflo.%s", w.group)

	w.logger.Info().
		Str("stream", stream).
		Str("subject", subject).
		Str("group", w.group).
		Msg("Starting worker")

	js := w.natsClient.JetStream()

	consumerName := fmt.Sprintf("pgflo_%s_consumer", w.group)

	consumerConfig := &nats.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     nats.AckExplicitPolicy,
		MaxDeliver:    1,
		AckWait:       25 * time.Minute,
		DeliverPolicy: nats.DeliverAllPolicy,
	}

	_, err := js.AddConsumer(stream, consumerConfig)
	if err != nil && !errors.Is(err, nats.ErrConsumerNameAlreadyInUse) {
		w.logger.Error().Err(err).Msg("Failed to add or update consumer")
		return fmt.Errorf("failed to add or update consumer: %w", err)
	}

	sub, err := js.PullSubscribe(subject, consumerName)
	if err != nil {
		w.logger.Error().Err(err).Msg("Failed to subscribe to subject")
		return fmt.Errorf("failed to subscribe to subject: %w", err)
	}

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if err := w.processMessages(ctx, sub); err != nil && err != context.Canceled {
			w.logger.Error().Err(err).Msg("Error processing messages")
		}
	}()

	<-ctx.Done()
	w.logger.Info().Msg("Received shutdown signal. Initiating graceful shutdown...")

	w.wg.Wait()
	w.logger.Debug().Msg("All goroutines finished")

	return w.flushBuffer()
}

// processMessages continuously processes messages from the NATS consumer.
func (w *Worker) processMessages(ctx context.Context, sub *nats.Subscription) error {
	flushTicker := time.NewTicker(w.flushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info().Msg("Flushing remaining messages")
			return w.flushBuffer()
		case <-flushTicker.C:
			if err := w.flushBuffer(); err != nil {
				w.logger.Error().Err(err).Msg("Failed to flush buffer on interval")
			}
		default:
			msgs, err := sub.Fetch(10, nats.MaxWait(500*time.Millisecond))
			if err != nil && !errors.Is(err, nats.ErrTimeout) {
				w.logger.Error().Err(err).Msg("Error fetching messages")
				continue
			}

			for _, msg := range msgs {
				if err := w.processMessage(msg); err != nil {
					w.logger.Error().Err(err).Msg("Failed to process message")
				}
				if err := msg.Ack(); err != nil {
					w.logger.Error().Err(err).Msg("Failed to acknowledge message")
				}
			}
			if len(w.buffer) >= w.batchSize {
				if err := w.flushBuffer(); err != nil {
					w.logger.Error().Err(err).Msg("Failed to flush buffer")
				}
			}
		}
	}
}

// processMessage handles a single message, applying rules, writing to the sink, and updating the last processed sequence.
func (w *Worker) processMessage(msg *nats.Msg) error {
	metadata, err := msg.Metadata()
	if err != nil {
		w.logger.Error().Err(err).Msg("Failed to get message metadata")
		return err
	}

	var cdcMessage utils.CDCMessage
	err = cdcMessage.UnmarshalBinary(msg.Data)
	if err != nil {
		w.logger.Error().Err(err).Msg("Failed to unmarshal message")
		return err
	}

	if w.ruleEngine != nil {
		processedMessage, err := w.ruleEngine.ApplyRules(&cdcMessage)
		if err != nil {
			w.logger.Error().Err(err).Msg("Failed to apply rules")
			return err
		}
		if processedMessage == nil {
			w.logger.Debug().Msg("Message filtered out by rules")
			return nil
		}
		cdcMessage = *processedMessage
	}

	if w.router != nil {
		routedMessage, err := w.router.ApplyRouting(&cdcMessage)
		if err != nil {
			w.logger.Error().Err(err).Msg("Failed to apply routing")
			return err
		}
		if routedMessage == nil {
			w.logger.Debug().Msg("Message filtered out by routing")
			return nil
		}
		cdcMessage = *routedMessage
	}

	w.buffer = append(w.buffer, &cdcMessage)
	w.lastSavedState = metadata.Sequence.Stream

	return nil
}

// flushBuffer writes the buffered messages to the sink and updates the last processed sequence.
func (w *Worker) flushBuffer() error {
	if len(w.buffer) == 0 {
		return nil
	}

	w.logger.Debug().
		Int("messages", len(w.buffer)).
		Int("batch_size", w.batchSize).
		Msg("Flushing buffer")

	err := w.sink.WriteBatch(w.buffer)
	if err != nil {
		w.logger.Error().Err(err).Msg("Failed to write batch to sink")
		return err
	}

	state, err := w.natsClient.GetState()
	if err != nil {
		w.logger.Error().Err(err).Msg("Failed to get current state")
		return err
	}

	state.LastProcessedSeq[w.group] = w.lastSavedState
	if err := w.natsClient.SaveState(state); err != nil {
		w.logger.Error().Err(err).Msg("Failed to save state")
		return err
	}

	w.buffer = w.buffer[:0]
	return nil
}
