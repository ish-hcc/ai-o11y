package nvidiagpureceiver

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
const typeStr = "nvidia_gpu"

var componentType = component.MustNewType(typeStr)

// NewFactory returns the receiver's factory.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(componentType,
		func() component.Config { return createDefaultConfig() },
		receiver.WithMetrics(createMetrics, component.StabilityLevelDevelopment))
}

func createMetrics(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*Config)
	s := &gpuScraper{source: newNvidiaSMI(c.NvidiaSMI), now: time.Now}
	var opts []scraper.Option
	if c.XID.Enabled {
		s.xid = newXIDCounter(c.XID.Path, set.Logger)
		opts = append(opts, scraper.WithStart(s.xid.Start), scraper.WithShutdown(s.xid.Shutdown))
	}
	sc, err := scraper.NewMetrics(s.scrape, opts...)
	if err != nil {
		return nil, err
	}
	return scraperhelper.NewMetricsController(&c.ControllerConfig, set, next,
		scraperhelper.AddMetricsScraper(componentType, sc))
}
