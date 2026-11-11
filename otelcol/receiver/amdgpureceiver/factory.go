package amdgpureceiver

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scraperhelper"
)

// typeStr is the receiver's type in collector configuration.
const typeStr = "amd_gpu"

var componentType = component.MustNewType(typeStr)

// NewFactory returns the receiver's factory.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(componentType,
		func() component.Config { return createDefaultConfig() },
		receiver.WithMetrics(createMetrics, component.StabilityLevelDevelopment))
}

func createMetrics(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*Config)
	s := &gpuScraper{source: newAMDSMI(c.AMDSMI), now: time.Now}
	sc, err := scraper.NewMetrics(s.scrape)
	if err != nil {
		return nil, err
	}
	return scraperhelper.NewMetricsController(&c.ControllerConfig, set, next,
		scraperhelper.AddMetricsScraper(componentType, sc))
}
