package generator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/weaveworks/common/user"
	"go.uber.org/atomic"

	"github.com/grafana/tempo/modules/generator/storage"
	"github.com/grafana/tempo/pkg/tempopb"
)

const (
	// ringAutoForgetUnhealthyPeriods is how many consecutive timeout periods an unhealthy instance
	// in the ring will be automatically removed.
	ringAutoForgetUnhealthyPeriods = 2

	// We use a safe default instead of exposing to config option to the user
	// in order to simplify the config.
	ringNumTokens = 256
)

var (
	ErrUnconfigured = errors.New("no metrics_generator.storage.path configured, metrics generator will be disabled")
	ErrReadOnly     = errors.New("metrics-generator is shutting down")
)

type Generator struct {
	services.Service

	cfg       *Config
	overrides metricsGeneratorOverrides

	ringLifecycler *ring.BasicLifecycler

	instancesMtx sync.RWMutex
	instances    map[string]*instance

	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher

	// When set to true, the generator will refuse incoming pushes
	// and will flush any remaining metrics.
	readOnly atomic.Bool

	reg    prometheus.Registerer
	logger log.Logger
}

// New makes a new Generator.
func New(cfg *Config, overrides metricsGeneratorOverrides, reg prometheus.Registerer, logger log.Logger) (*Generator, error) {
	if cfg.Storage.Path == "" {
		return nil, ErrUnconfigured
	}

	err := os.MkdirAll(cfg.Storage.Path, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("failed to mkdir on %s: %w", cfg.Storage.Path, err)
	}

	g := &Generator{
		cfg:       cfg,
		overrides: overrides,

		instances: map[string]*instance{},

		reg:    reg,
		logger: logger,
	}

	// Lifecycler and ring
	ringStore, err := kv.NewClient(
		cfg.Ring.KVStore,
		ring.GetCodec(),
		kv.RegistererWithKVName(prometheus.WrapRegistererWithPrefix("tempo_", reg), "metrics-generator"),
		g.logger,
	)
	if err != nil {
		return nil, fmt.Errorf("create KV store client: %w", err)
	}

	lifecyclerCfg, err := cfg.Ring.toLifecyclerConfig()
	if err != nil {
		return nil, fmt.Errorf("invalid ring lifecycler config: %w", err)
	}

	// Define lifecycler delegates in reverse order (last to be called defined first because they're
	// chained via "next delegate").
	delegate := ring.BasicLifecyclerDelegate(g)
	delegate = ring.NewLeaveOnStoppingDelegate(delegate, g.logger)
	delegate = ring.NewAutoForgetDelegate(ringAutoForgetUnhealthyPeriods*cfg.Ring.HeartbeatTimeout, delegate, g.logger)

	g.ringLifecycler, err = ring.NewBasicLifecycler(lifecyclerCfg, ringNameForServer, RingKey, ringStore, delegate, g.logger, prometheus.WrapRegistererWithPrefix("tempo_", reg))
	if err != nil {
		return nil, fmt.Errorf("create ring lifecycler: %w", err)
	}

	g.Service = services.NewBasicService(g.starting, g.running, g.stopping)
	return g, nil
}

func (g *Generator) starting(ctx context.Context) (err error) {
	// In case this function will return error we want to unregister the instance
	// from the ring. We do it ensuring dependencies are gracefully stopped if they
	// were already started.
	defer func() {
		if err == nil || g.subservices == nil {
			return
		}

		if stopErr := services.StopManagerAndAwaitStopped(context.Background(), g.subservices); stopErr != nil {
			level.Error(g.logger).Log("msg", "failed to gracefully stop metrics-generator dependencies", "err", stopErr)
		}
	}()

	g.subservices, err = services.NewManager(g.ringLifecycler)
	if err != nil {
		return fmt.Errorf("unable to start metrics-generator dependencies: %w", err)
	}
	g.subservicesWatcher = services.NewFailureWatcher()
	g.subservicesWatcher.WatchManager(g.subservices)

	err = services.StartManagerAndAwaitHealthy(ctx, g.subservices)
	if err != nil {
		return fmt.Errorf("unable to start mertics-generator dependencies: %w", err)
	}

	return nil
}

func (g *Generator) running(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil

		case err := <-g.subservicesWatcher.Chan():
			return fmt.Errorf("metrics-generator subservice failed %w", err)
		}
	}
}

func (g *Generator) stopping(_ error) error {
	// Mark as read-only
	g.stopIncomingRequests()

	if g.subservices != nil {
		err := services.StopManagerAndAwaitStopped(context.Background(), g.subservices)
		if err != nil {
			level.Error(g.logger).Log("msg", "failed to stop metrics-generator dependencies", "err", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(len(g.instances))

	for _, inst := range g.instances {
		go func(inst *instance) {
			inst.shutdown()
			wg.Done()
		}(inst)
	}

	wg.Wait()

	return nil
}

// stopIncomingRequests marks the generator as read-only, refusing push requests
func (g *Generator) stopIncomingRequests() {
	g.readOnly.Store(true)
}

func (g *Generator) PushSpans(ctx context.Context, req *tempopb.PushSpansRequest) (*tempopb.PushResponse, error) {
	if g.readOnly.Load() {
		return nil, ErrReadOnly
	}

	span, ctx := opentracing.StartSpanFromContext(ctx, "generator.PushSpans")
	defer span.Finish()

	instanceID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}
	span.SetTag("instanceID", instanceID)

	instance, err := g.getOrCreateInstance(instanceID)
	if err != nil {
		return nil, err
	}

	instance.pushSpans(ctx, req)

	return &tempopb.PushResponse{}, nil
}

func (g *Generator) getOrCreateInstance(instanceID string) (*instance, error) {
	inst, ok := g.getInstanceByID(instanceID)
	if ok {
		return inst, nil
	}

	g.instancesMtx.Lock()
	defer g.instancesMtx.Unlock()

	inst, ok = g.instances[instanceID]
	if ok {
		return inst, nil
	}

	inst, err := g.createInstance(instanceID)
	if err != nil {
		return nil, err
	}
	g.instances[instanceID] = inst
	return inst, nil
}

func (g *Generator) getInstanceByID(id string) (*instance, bool) {
	g.instancesMtx.RLock()
	defer g.instancesMtx.RUnlock()

	inst, ok := g.instances[id]
	return inst, ok
}

func (g *Generator) createInstance(id string) (*instance, error) {
	wal, err := storage.New(&g.cfg.Storage, id, g.reg, g.logger)
	if err != nil {
		return nil, err
	}

	return newInstance(g.cfg, id, g.overrides, wal, g.reg, g.logger)
}

func (g *Generator) CheckReady(_ context.Context) error {
	if !g.ringLifecycler.IsRegistered() {
		return fmt.Errorf("metrics-generator check ready failed: not registered in the ring")
	}

	return nil
}

// OnRingInstanceRegister implements ring.BasicLifecyclerDelegate
func (g *Generator) OnRingInstanceRegister(lifecycler *ring.BasicLifecycler, ringDesc ring.Desc, instanceExists bool, instanceID string, instanceDesc ring.InstanceDesc) (ring.InstanceState, ring.Tokens) {
	// When we initialize the metrics-generator instance in the ring we want to start from
	// a clean situation, so whatever is the state we set it ACTIVE, while we keep existing
	// tokens (if any) or the ones loaded from file.
	var tokens []uint32
	if instanceExists {
		tokens = instanceDesc.GetTokens()
	}

	takenTokens := ringDesc.GetTokens()
	newTokens := ring.GenerateTokens(ringNumTokens-len(tokens), takenTokens)

	// Tokens sorting will be enforced by the parent caller.
	tokens = append(tokens, newTokens...)

	return ring.ACTIVE, tokens
}

// OnRingInstanceTokens implements ring.BasicLifecyclerDelegate
func (g *Generator) OnRingInstanceTokens(lifecycler *ring.BasicLifecycler, tokens ring.Tokens) {
}

// OnRingInstanceStopping implements ring.BasicLifecyclerDelegate
func (g *Generator) OnRingInstanceStopping(lifecycler *ring.BasicLifecycler) {
}

// OnRingInstanceHeartbeat implements ring.BasicLifecyclerDelegate
func (g *Generator) OnRingInstanceHeartbeat(lifecycler *ring.BasicLifecycler, ringDesc *ring.Desc, instanceDesc *ring.InstanceDesc) {
}
