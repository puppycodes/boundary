package event

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/eventlogger"
	"github.com/hashicorp/eventlogger/filters/gated"
	"github.com/hashicorp/eventlogger/sinks/writer"
	"github.com/hashicorp/go-hclog"
)

const (
	OpField          = "op"           // OpField in an event.
	RequestInfoField = "request_info" // RequestInfoField in an event.
	VersionField     = "version"      // VersionField in an event
	DetailsField     = "details"      // Details field in an event.
	HeaderField      = "header"       // HeaderField in an event.
	IdField          = "id"           // IdField in an event.
	CreatedAtField   = "created_at"   // CreatedAtField in an event.
	TypeField        = "type"         // TypeField in an event.

	auditPipeline       = "audit-pipeline"       // auditPipeline is a pipeline for audit events
	observationPipeline = "observation-pipeline" // observationPipeline is a pipeline for observation events
	errPipeline         = "err-pipeline"         // errPipeline is a pipeline for error events
	sysPipeline         = "sys-pipeline"         // sysPipeline is a pipeline for system events
)

// flushable defines an interface that all eventlogger Nodes must implement if
// they are "flushable"
type flushable interface {
	FlushAll(ctx context.Context) error
}

// broker defines an interface for an eventlogger Broker... which will allow us
// to substitute our testing broker when needed to write tests for things
// like event send retrying.
type broker interface {
	Send(ctx context.Context, t eventlogger.EventType, payload interface{}) (eventlogger.Status, error)
	Reopen(ctx context.Context) error
	StopTimeAt(now time.Time)
	RegisterNode(id eventlogger.NodeID, node eventlogger.Node) error
	SetSuccessThreshold(t eventlogger.EventType, successThreshold int) error
	RegisterPipeline(def eventlogger.Pipeline) error
}

// Eventer provides a method to send events to pipelines of sinks
type Eventer struct {
	broker               broker
	flushableNodes       []flushable
	conf                 EventerConfig
	logger               hclog.Logger
	auditPipelines       []pipeline
	observationPipelines []pipeline
	errPipelines         []pipeline
}

type pipeline struct {
	eventType  Type
	fmtId      eventlogger.NodeID
	sinkId     eventlogger.NodeID
	gateId     eventlogger.NodeID
	sinkConfig SinkConfig
}

var (
	sysEventer     *Eventer     // sysEventer is the system-wide Eventer
	sysEventerLock sync.RWMutex // sysEventerLock allows the sysEventer to safely be written concurrently.
)

// InitSysEventer provides a mechanism to initialize a "system wide" eventer
// singleton for Boundary.  Support the options of: WithEventer(...) and
// WithEventerConfig(...)
//
// IMPORTANT: Eventers cannot share file sinks, which likely means that each
// process should only have one Eventer.  In practice this means the process
// Server (Controller or Worker) and the SysEventer both need a pointer to a
// single Eventer.
func InitSysEventer(log hclog.Logger, serializationLock *sync.Mutex, opt ...Option) error {
	const op = "event.InitSysEventer"
	if log == nil {
		return fmt.Errorf("%s: missing hclog: %w", op, ErrInvalidParameter)
	}
	if serializationLock == nil {
		return fmt.Errorf("%s: missing serialization lock: %w", op, ErrInvalidParameter)
	}

	// the order of operations is important here.  we want to determine if
	// there's an error before setting the singleton sysEventer
	var e *Eventer
	opts := getOpts(opt...)
	switch {
	case opts.withEventer == nil && opts.withEventerConfig == nil:
		return fmt.Errorf("%s: missing both eventer and eventer config: %w", op, ErrInvalidParameter)

	case opts.withEventer != nil && opts.withEventerConfig != nil:
		return fmt.Errorf("%s: both eventer and eventer config provided: %w", op, ErrInvalidParameter)

	case opts.withEventerConfig != nil:
		var err error
		if e, err = NewEventer(log, serializationLock, *opts.withEventerConfig); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}

	case opts.withEventer != nil:
		e = opts.withEventer
	}

	sysEventerLock.Lock()
	defer sysEventerLock.Unlock()
	sysEventer = e
	return nil
}

// SysEventer returns the "system wide" eventer for Boundary and can/will return
// a nil Eventer
func SysEventer() *Eventer {
	sysEventerLock.RLock()
	defer sysEventerLock.RUnlock()
	return sysEventer
}

// NewEventer creates a new Eventer using the config.  Supports options:
// WithNow, WithSerializationLock, WithBroker
func NewEventer(log hclog.Logger, serializationLock *sync.Mutex, c EventerConfig, opt ...Option) (*Eventer, error) {
	const op = "event.NewEventer"
	if log == nil {
		return nil, fmt.Errorf("%s: missing logger: %w", op, ErrInvalidParameter)
	}
	if serializationLock == nil {
		return nil, fmt.Errorf("%s: missing serialization lock: %w", op, ErrInvalidParameter)
	}

	// if there are no sinks in config, then we'll default to just one stderr
	// sink.
	if len(c.Sinks) == 0 {
		c.Sinks = append(c.Sinks, DefaultSink())
	}

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	var auditPipelines, observationPipelines, errPipelines, sysPipelines []pipeline

	opts := getOpts(opt...)
	var b broker
	switch {
	case opts.withBroker != nil:
		b = opts.withBroker
	default:
		b = eventlogger.NewBroker()
	}

	e := &Eventer{
		logger: log,
		conf:   c,
		broker: b,
	}

	if !opts.withNow.IsZero() {
		e.broker.StopTimeAt(opts.withNow)
	}

	// Create JSONFormatter node
	id, err := newId("json")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	jsonfmtId := eventlogger.NodeID(id)
	fmtNode := &eventlogger.JSONFormatter{}
	err = e.broker.RegisterNode(jsonfmtId, fmtNode)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to register json node: %w", op, err)
	}

	// serializedStderr will be shared among all StderrSinks so their output is not
	// interwoven
	serializedStderr := serializedWriter{
		w: os.Stderr,
		l: serializationLock,
	}

	// we need to keep track of all the Sink filenames to ensure they aren't
	// reused.
	allSinkFilenames := map[string]bool{}

	for _, s := range c.Sinks {
		var sinkId eventlogger.NodeID
		var sinkNode eventlogger.Node
		switch s.SinkType {
		case StderrSink:
			sinkNode = &writer.Sink{
				Format: string(s.Format),
				Writer: &serializedStderr,
			}
			id, err = newId("stderr")
			if err != nil {
				return nil, fmt.Errorf("%s: %w", op, err)
			}
			sinkId = eventlogger.NodeID(id)
		default:
			if _, found := allSinkFilenames[s.Path+s.FileName]; found {
				return nil, fmt.Errorf("%s: duplicate file sink: %s %s", op, s.Path, s.FileName)
			}
			sinkNode = &eventlogger.FileSink{
				Format:      string(s.Format),
				Path:        s.Path,
				FileName:    s.FileName,
				MaxBytes:    s.RotateBytes,
				MaxDuration: s.RotateDuration,
				MaxFiles:    s.RotateMaxFiles,
			}
			id, err = newId(fmt.Sprintf("file_%s_%s_", s.Path, s.FileName))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", op, err)
			}
			sinkId = eventlogger.NodeID(id)
		}
		err = e.broker.RegisterNode(sinkId, sinkNode)
		if err != nil {
			return nil, fmt.Errorf("%s: failed to register sink node %s: %w", op, sinkId, err)
		}
		var addToAudit, addToObservation, addToErr, addToSys bool
		for _, t := range s.EventTypes {
			switch t {
			case EveryType:
				addToAudit = true
				addToObservation = true
				addToErr = true
				addToSys = true
			case ErrorType:
				addToErr = true
			case AuditType:
				addToAudit = true
			case ObservationType:
				addToObservation = true
			case SystemType:
				addToSys = true
			}
		}
		if addToAudit {
			auditPipelines = append(auditPipelines, pipeline{
				eventType:  AuditType,
				fmtId:      jsonfmtId,
				sinkId:     sinkId,
				sinkConfig: s,
			})
		}
		if addToObservation {
			observationPipelines = append(observationPipelines, pipeline{
				eventType:  ObservationType,
				fmtId:      jsonfmtId,
				sinkId:     sinkId,
				sinkConfig: s,
			})
		}
		if addToErr {
			errPipelines = append(errPipelines, pipeline{
				eventType:  ErrorType,
				fmtId:      jsonfmtId,
				sinkId:     sinkId,
				sinkConfig: s,
			})
		}
		if addToSys {
			sysPipelines = append(sysPipelines, pipeline{
				eventType: SystemType,
				fmtId:     jsonfmtId,
				sinkId:    sinkId,
			})
		}
	}
	if c.AuditEnabled && len(auditPipelines) == 0 {
		return nil, fmt.Errorf("%s: audit events enabled but no sink defined for it: %w", op, ErrInvalidParameter)
	}
	if c.ObservationsEnabled && len(observationPipelines) == 0 {
		return nil, fmt.Errorf("%s: observation events enabled but no sink defined for it: %w", op, ErrInvalidParameter)
	}
	if c.SysEventsEnabled && len(sysPipelines) == 0 {
		return nil, fmt.Errorf("%s: system events enabled but no sink defined for it: %w", op, ErrInvalidParameter)
	}

	for _, p := range auditPipelines {
		gatedFilterNode := gated.Filter{
			Broker: e.broker,
		}
		e.flushableNodes = append(e.flushableNodes, &gatedFilterNode)
		gateId, err := newId("gated-audit")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		p.gateId = eventlogger.NodeID(gateId)
		if err := e.broker.RegisterNode(p.gateId, &gatedFilterNode); err != nil {
			return nil, fmt.Errorf("%s: unable to register audit gated filter: %w", op, err)
		}

		pipeId, err := newId(auditPipeline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		err = e.broker.RegisterPipeline(eventlogger.Pipeline{
			EventType:  eventlogger.EventType(p.eventType),
			PipelineID: eventlogger.PipelineID(pipeId),
			NodeIDs:    []eventlogger.NodeID{p.gateId, p.fmtId, p.sinkId},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: failed to register audit pipeline: %w", op, err)
		}
	}

	for _, p := range observationPipelines {
		gatedFilterNode := gated.Filter{
			Broker: e.broker,
		}
		e.flushableNodes = append(e.flushableNodes, &gatedFilterNode)
		gateId, err := newId("gated-observation")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		p.gateId = eventlogger.NodeID(gateId)
		if err := e.broker.RegisterNode(p.gateId, &gatedFilterNode); err != nil {
			return nil, fmt.Errorf("%s: unable to register audit gated filter: %w", op, err)
		}

		pipeId, err := newId(observationPipeline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		err = e.broker.RegisterPipeline(eventlogger.Pipeline{
			EventType:  eventlogger.EventType(p.eventType),
			PipelineID: eventlogger.PipelineID(pipeId),
			NodeIDs:    []eventlogger.NodeID{p.gateId, p.fmtId, p.sinkId},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: failed to register observation pipeline: %w", op, err)
		}
	}
	errNodeIds := make([]eventlogger.NodeID, 0, len(errPipelines))
	for _, p := range errPipelines {
		pipeId, err := newId(errPipeline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		err = e.broker.RegisterPipeline(eventlogger.Pipeline{
			EventType:  eventlogger.EventType(p.eventType),
			PipelineID: eventlogger.PipelineID(pipeId),
			NodeIDs:    []eventlogger.NodeID{p.fmtId, p.sinkId},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: failed to register err pipeline: %w", op, err)
		}
		errNodeIds = append(errNodeIds, p.sinkId)
	}
	sysNodeIds := make([]eventlogger.NodeID, 0, len(sysPipelines))
	for _, p := range sysPipelines {
		pipeId, err := newId(sysPipeline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		err = e.broker.RegisterPipeline(eventlogger.Pipeline{
			EventType:  eventlogger.EventType(p.eventType),
			PipelineID: eventlogger.PipelineID(pipeId),
			NodeIDs:    []eventlogger.NodeID{p.fmtId, p.sinkId},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: failed to register sys pipeline: %w", op, err)
		}
		sysNodeIds = append(sysNodeIds, p.sinkId)
	}

	// always enforce delivery of errors
	err = e.broker.SetSuccessThreshold(eventlogger.EventType(ErrorType), len(errNodeIds))
	if err != nil {
		return nil, fmt.Errorf("%s: failed to set success threshold for error events: %w", op, err)
	}

	e.auditPipelines = append(e.auditPipelines, auditPipelines...)
	e.errPipelines = append(e.errPipelines, errPipelines...)
	e.observationPipelines = append(e.observationPipelines, observationPipelines...)

	return e, nil
}

func DefaultEventerConfig() *EventerConfig {
	return &EventerConfig{
		AuditEnabled:        false,
		ObservationsEnabled: true,
		SysEventsEnabled:    true,
		Sinks:               []SinkConfig{DefaultSink()},
	}
}

func DefaultSink() SinkConfig {
	return SinkConfig{
		Name:       "default",
		EventTypes: []Type{EveryType},
		Format:     JSONSinkFormat,
		SinkType:   StderrSink,
	}
}

// writeObservation writes/sends an Observation event.
func (e *Eventer) writeObservation(ctx context.Context, event *observation) error {
	const op = "event.(Eventer).writeObservation"
	if event == nil {
		return fmt.Errorf("%s: missing event: %w", op, ErrInvalidParameter)
	}
	if !e.conf.ObservationsEnabled {
		return nil
	}
	err := e.retrySend(ctx, stdRetryCount, expBackoff{}, func() (eventlogger.Status, error) {
		if event.Header != nil {
			event.Header[RequestInfoField] = event.RequestInfo
			event.Header[VersionField] = event.Version
		}
		if event.Detail != nil {
			event.Detail[OpField] = string(event.Op)
		}
		return e.broker.Send(ctx, eventlogger.EventType(ObservationType), event.Payload)
	})
	if err != nil {
		e.logger.Error("encountered an error sending an observation event", "error:", err.Error())
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// writeError writes/sends an Err event
func (e *Eventer) writeError(ctx context.Context, event *err) error {
	const op = "event.(Eventer).writeError"
	if event == nil {
		return fmt.Errorf("%s: missing event: %w", op, ErrInvalidParameter)
	}
	err := e.retrySend(ctx, stdRetryCount, expBackoff{}, func() (eventlogger.Status, error) {
		return e.broker.Send(ctx, eventlogger.EventType(ErrorType), event)
	})
	if err != nil {
		e.logger.Error("encountered an error sending an error event", "error:", err.Error())
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// writeSysEvent writes/sends an sysEvent event
func (e *Eventer) writeSysEvent(ctx context.Context, event *sysEvent) error {
	const op = "event.(Eventer).writeSysEvent"
	if event == nil {
		return fmt.Errorf("%s: missing event: %w", op, ErrInvalidParameter)
	}
	err := e.retrySend(ctx, stdRetryCount, expBackoff{}, func() (eventlogger.Status, error) {
		return e.broker.Send(ctx, eventlogger.EventType(SystemType), event)
	})
	if err != nil {
		e.logger.Error("encountered an error sending an sys event", "error:", err.Error())
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// writeAudit writes/send an audit event
func (e *Eventer) writeAudit(ctx context.Context, event *audit) error {
	const op = "event.(Eventer).writeAudit"
	if event == nil {
		return fmt.Errorf("%s: missing event: %w", op, ErrInvalidParameter)
	}
	if !e.conf.AuditEnabled {
		return nil
	}
	err := e.retrySend(ctx, stdRetryCount, expBackoff{}, func() (eventlogger.Status, error) {
		return e.broker.Send(ctx, eventlogger.EventType(AuditType), event)
	})
	if err != nil {
		e.logger.Error("encountered an error sending an audit event", "error:", err.Error())
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// Reopen can used during a SIGHUP to reopen nodes, most importantly the underlying
// file sinks.
func (e *Eventer) Reopen() error {
	if e.broker != nil {
		return e.broker.Reopen(context.Background())
	}
	return nil
}

// FlushNodes will flush any of the eventer's flushable nodes.  This
// needs to be called whenever Boundary is stopping (aka shutting down).
func (e *Eventer) FlushNodes(ctx context.Context) error {
	const op = "event.(Eventer).FlushNodes"
	for _, n := range e.flushableNodes {
		if err := n.FlushAll(ctx); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	return nil
}
