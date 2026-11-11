package topprocessprocessor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

// typeStr is the processor's type in collector configuration.
const typeStr = "top_process"

var componentType = component.MustNewType(typeStr)

// NewFactory returns the processor's factory.
func NewFactory() processor.Factory {
	return processor.NewFactory(componentType,
		createDefaultConfig,
		processor.WithMetrics(createMetrics, component.StabilityLevelDevelopment))
}

func createDefaultConfig() component.Config {
	return &Config{TopN: 10, By: []string{ByCPU, ByMemory}}
}

func createMetrics(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
	p := newProcessor(cfg.(*Config))
	return processorhelper.NewMetrics(ctx, set, cfg, next, p.processMetrics,
		processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}))
}
