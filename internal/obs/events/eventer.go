package event

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/boundary/internal/errors"
	"github.com/hashicorp/eventlogger"
	"github.com/hashicorp/go-hclog"
)

type SinkType string
type SinkFormat string
type DeliveryGuarantee string

const (
	FileSink       SinkType          = "file"
	JSONSinkFormat SinkFormat        = "json"
	Enforced       DeliveryGuarantee = "enforced"
	BestEffort     DeliveryGuarantee = "best-effort"
	AuditPipeline                    = "audit-pipeline"
	InfoPipeline                     = "info-pipeline"
	ErrPipeline                      = "err-pipeline"
)

type Eventer struct {
	broker *eventlogger.Broker
	conf   Config
	logger hclog.Logger
	l      sync.Mutex
}

type SinkConfig struct {
	Name           string
	EventTypes     []Type
	SinkType       SinkType
	Format         SinkFormat
	Path           string
	FileName       string
	RotateBytes    int
	RotateDuration time.Duration
	RotateMaxFiles int
}

type Config struct {
	AuditDelivery DeliveryGuarantee
	InfoDelivery  DeliveryGuarantee
	AuditEnabled  bool
	InfoEnabled   bool
	Sinks         []SinkConfig
}

func NewEventer(log hclog.Logger, c Config) (*Eventer, error) {
	const op = "event.NewEventer"
	if log == nil {
		return nil, errors.New(errors.InvalidParameter, op, "missing logger")
	}
	var auditNodeIds, infoNodeIds, errNodeIds []eventlogger.NodeID
	broker := eventlogger.NewBroker()

	// Create JSONFormatter node
	id, err := newId("json")
	if err != nil {
		return nil, errors.Wrap(err, op)
	}
	jsonfmtID := eventlogger.NodeID(id)
	fmtNode := &eventlogger.JSONFormatter{}
	err = broker.RegisterNode(jsonfmtID, fmtNode)
	if err != nil {
		return nil, errors.Wrap(err, "failed to register json node")
	}
	auditNodeIds = append(auditNodeIds, jsonfmtID)
	infoNodeIds = append(infoNodeIds, jsonfmtID)
	errNodeIds = append(errNodeIds, jsonfmtID)

	if len(c.Sinks) == 0 {
		c.Sinks = append(c.Sinks, SinkConfig{
			Name:       "default",
			EventTypes: []Type{EveryType},
			Format:     JSONSinkFormat,
			Path:       "./",
			FileName:   "foo.txt",
		})
	}

	for _, s := range c.Sinks {
		fileSinkNode := eventlogger.FileSink{
			Format:      string(s.Format),
			Path:        s.Path,
			FileName:    s.FileName,
			MaxBytes:    s.RotateBytes,
			MaxDuration: s.RotateDuration,
			MaxFiles:    s.RotateMaxFiles,
		}

		id, err = newId(fmt.Sprintf("file_%s_%s_", s.Path, s.FileName))
		if err != nil {
			return nil, errors.Wrap(err, op)
		}
		sinkID := eventlogger.NodeID(id)
		err = broker.RegisterNode(sinkID, &fileSinkNode)
		if err != nil {
			return nil, errors.Wrap(err, "failed to register json node")
		}
		var addToAudit, addToInfo, addToErr bool
		for _, t := range s.EventTypes {
			switch t {
			case EveryType:
				addToAudit = true
				addToInfo = true
				addToErr = true
			case ErrorType:
				addToErr = true
			case AuditType:
				addToAudit = true
			case InfoType:
				addToInfo = true
			}
		}
		if addToAudit {
			auditNodeIds = append(auditNodeIds, sinkID)
		}
		if addToInfo {
			infoNodeIds = append(infoNodeIds, sinkID)
		}
		if addToErr {
			errNodeIds = append(errNodeIds, sinkID)
		}
	}

	// Register pipeline to broker for audit events
	err = broker.RegisterPipeline(eventlogger.Pipeline{
		EventType:  eventlogger.EventType(AuditType),
		PipelineID: AuditPipeline,
		NodeIDs:    auditNodeIds,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to register audit pipeline")
	}

	// Register pipeline to broker for info events
	err = broker.RegisterPipeline(eventlogger.Pipeline{
		EventType:  eventlogger.EventType(InfoType),
		PipelineID: InfoPipeline,
		NodeIDs:    infoNodeIds,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to register info pipeline")
	}

	// Register pipeline to broker for err events
	err = broker.RegisterPipeline(eventlogger.Pipeline{
		EventType:  eventlogger.EventType(ErrorType),
		PipelineID: ErrPipeline,
		NodeIDs:    errNodeIds,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to register error pipeline")
	}

	// TODO(jimlambrt) go-eventlogger SetSuccessThreshold currently does not
	// specify which sink passed and which hasn't so we are unable to
	// support multiple sinks with different delivery guarantees
	if c.AuditDelivery == Enforced {
		err = broker.SetSuccessThreshold(eventlogger.EventType(AuditType), len(auditNodeIds))
		if err != nil {
			return nil, errors.Wrap(err, "failed to set success threshold for audit events")
		}
	}
	if c.InfoDelivery == Enforced {
		err = broker.SetSuccessThreshold(eventlogger.EventType(InfoType), len(infoNodeIds))
		if err != nil {
			return nil, errors.Wrap(err, "failed to set success threshold for info events")
		}
	}
	// always enforce delivery of errors
	err = broker.SetSuccessThreshold(eventlogger.EventType(ErrorType), len(errNodeIds))
	if err != nil {
		return nil, errors.Wrap(err, "failed to set success threshold for error events")
	}

	return &Eventer{
		logger: log,
		conf:   c,
		broker: broker,
	}, nil
}

func (e *Eventer) Info(ctx context.Context, event *Info, opt ...Option) error {
	const op = "event.(Eventer).Info"
	if !e.conf.InfoEnabled {
		return nil
	}
	status, err := e.broker.Send(ctx, eventlogger.EventType(InfoType), event)
	if err != nil {
		e.logger.Error("encountered an error sending an info event", "error:", err.Error())
		return errors.Wrap(err, op)
	}
	if len(status.Warnings) > 0 {
		e.logger.Warn("encountered warnings send info event", "warnings:", status.Warnings)
	}
	return nil
}

func (e *Eventer) Error(ctx context.Context, event *Err, opt ...Option) error {
	const op = "event.(Eventer).Error"
	status, err := e.broker.Send(ctx, eventlogger.EventType(ErrorType), event)
	if err != nil {
		e.logger.Error("encountered an error sending an error event", "error:", err.Error())
		return errors.Wrap(err, op)
	}
	if len(status.Warnings) > 0 {
		e.logger.Warn("encountered warnings send error event", "warnings:", status.Warnings)
	}
	return nil
}

func (e *Eventer) Audit(ctx context.Context, event *Audit, opt ...Option) error {
	const op = "event.(Eventer).Audit"
	if !e.conf.AuditEnabled {
		return nil
	}
	status, err := e.broker.Send(ctx, eventlogger.EventType(InfoType), event)
	if err != nil {
		e.logger.Error("encountered an error sending an audit event", "error:", err.Error())
		return errors.Wrap(err, op)
	}
	if len(status.Warnings) > 0 {
		e.logger.Warn("encountered warnings send audit event", "warnings:", status.Warnings)
	}
	return nil
}

// Reopen is used during a SIGHUP to reopen nodes, most importantly the underlying
// file sinks.
func (e *Eventer) Reopen() error {
	e.l.Lock()
	defer e.l.Unlock()
	return e.broker.Reopen(context.Background())
}

// SetAuditEnabled sets the auditor to enabled or disabled
func (e *Eventer) SetAuditEnabled(enabled bool) {
	e.l.Lock()
	defer e.l.Unlock()

	e.conf.AuditEnabled = enabled
}

// SetInfoEnabled sets the info to enabled or disabled
func (e *Eventer) SetInfoEnabled(enabled bool) {
	e.l.Lock()
	defer e.l.Unlock()

	e.conf.InfoEnabled = enabled
}

// AuditDeliveryGuaranteed allows callers to determine the guarantee of audit
// event delivery.
func (e *Eventer) AuditDeliveryGuaranteed() bool {
	return e.conf.AuditDelivery == Enforced
}

// InfoDeliveryGuaranteed allows callers to determine the guarantee of info
// event delivery.
func (e *Eventer) InfoDeliveryGuaranteed() bool {
	return e.conf.InfoDelivery == Enforced
}
