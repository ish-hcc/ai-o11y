package rest

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"

	"github.com/cloud-barista/ai-o11y/internal/model"
	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/backend/promapi"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/query"
)

// RestGetAllMetrics godoc
// @ID GetAllMetrics
// @Summary List metrics
// @Description List the metric catalog: the common fields, named and meant as OpenTelemetry names them, and the fields of one collector's own, each with the attribute that splits it and, for a field no collector provides yet, the reason.
// @Tags [Monitoring] Metrics
// @Produce json
// @Success 200 {object} model.MetricListResp
// @Security BearerAuth
// @Failure 401 {object} model.SimpleMsg "Missing or wrong bearer token"
// @Router /metrics [get]
func (h *handler) RestGetAllMetrics(c echo.Context) error {
	rows := h.Catalog.Rows()
	resp := model.MetricListResp{Metrics: make([]model.MetricInfo, 0, len(rows))}
	for _, r := range rows {
		resp.Metrics = append(resp.Metrics, model.MetricInfo{
			Name: r.Name, Schema: r.Schema, Area: r.Area, Unit: r.Unit, Kind: string(r.Kind),
			Description: r.Description, Attribute: attributeInfo(r), Dimension: r.DimensionName,
			Requires: r.Requires, Supported: r.Gap == "", Gap: r.Gap, ZeroFill: len(r.ZeroFill) > 0,
		})
	}
	return c.JSON(http.StatusOK, resp)
}

func attributeInfo(r monitoring.MetricRow) *model.AttributeInfo {
	if r.Attribute == nil {
		return nil
	}
	return &model.AttributeInfo{Name: r.Attribute.Name, Values: r.Attribute.Values}
}

// RestGetTargetMetrics godoc
// @ID GetTargetMetrics
// @Summary List a target's metrics
// @Description List the metric catalog as it stands for one target: which fields it has, from the collectors that run on it, and which it does not, with the reason.
// @Tags [Monitoring] Metrics
// @Produce json
// @Param targetId path string true "Target id (CB-Tumblebug node Uid)"
// @Success 200 {object} model.TargetMetricListResp
// @Failure 404 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Failure 503 {object} model.SimpleMsg
// @Security BearerAuth
// @Failure 401 {object} model.SimpleMsg "Missing or wrong bearer token"
// @Router /targets/{targetId}/metrics [get]
func (h *handler) RestGetTargetMetrics(c echo.Context) error {
	t, ok, err := h.lookup(c)
	if !ok {
		return err
	}
	running := map[string]bool{}
	for _, k := range h.Collectors {
		if k.AppliesTo(t) {
			running[k.Name()] = true
		}
	}
	rows := h.Catalog.Rows()
	gpuRead := false
	for _, r := range rows {
		if r.Area == "gpu" && providedOn(r, running) {
			gpuRead = true
			break
		}
	}
	resp := model.TargetMetricListResp{TargetID: t.EntityID, Metrics: make([]model.TargetMetricInfo, 0, len(rows))}
	for _, r := range rows {
		reason := unsupported(r, t, running, gpuRead)
		resp.Metrics = append(resp.Metrics, model.TargetMetricInfo{
			Name: r.Name, Schema: r.Schema, Area: r.Area, Unit: r.Unit,
			Attribute: attributeInfo(r), Dimension: r.DimensionName,
			Supported: reason == "", Reason: reason,
		})
	}
	return c.JSON(http.StatusOK, resp)
}

// unsupported says why target t does not have the field of row, or "" when it
// does: no collector provides it yet, none of the collectors provide it on t,
// or t lacks what it requires. gpuRead says a collector reads some GPU field
// on t. A GPU is known by its count or by its model alone, as the collectors
// decide (CB-Tumblebug leaves the count of some specs out).
func unsupported(r monitoring.MetricRow, t monitoring.Target, running map[string]bool, gpuRead bool) string {
	if r.Gap != "" {
		return r.Gap
	}
	if !providedOn(r, running) {
		hasGPU := t.GPUCount > 0 || t.GPUModel != ""
		switch {
		case r.Area == "gpu" && hasGPU && !gpuRead:
			return "The target's GPU model names neither NVIDIA nor AMD, the only GPU vendors read yet"
		case r.Area == "gpu" && hasGPU:
			return "The collector for the target's GPU does not provide it"
		}
		return "No collector running on this target provides it"
	}
	for _, req := range r.Requires {
		os, isOS := strings.CutPrefix(req, "os:")
		switch {
		case isOS && t.OS == "":
			return "Needs " + os + "; the target's OS is unknown"
		case isOS && t.OS != os:
			return "Needs " + os
		case !isOS:
			return "Needs " + req + ", which is not known for the target"
		}
	}
	return ""
}

// providedOn reports whether one of the running collectors provides row r.
func providedOn(r monitoring.MetricRow, running map[string]bool) bool {
	for collector := range r.Exprs {
		if running[collector] {
			return true
		}
	}
	return false
}

// RestPostQuery godoc
// @ID PostQuery
// @Summary Query a metric
// @Description Query one metric over targets named by id or selected by their attributes, as one value per series or as a graph from start to end. The answer carries the series and the targets they belong to.
// @Tags [Monitoring] Metrics
// @Accept json
// @Produce json
// @Param queryReq body model.QueryReq true "Query"
// @Success 200 {object} model.QueryResp
// @Failure 400 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg "Unknown metric or target"
// @Failure 422 {object} model.SimpleMsg "Metric is not supported"
// @Failure 500 {object} model.SimpleMsg
// @Failure 502 {object} model.SimpleMsg "The metrics backend did not answer"
// @Failure 503 {object} model.SimpleMsg
// @Security BearerAuth
// @Failure 401 {object} model.SimpleMsg "Missing or wrong bearer token"
// @Router /query [post]
func (h *handler) RestPostQuery(c echo.Context) error {
	ctx := c.Request().Context()
	if h.Backend == nil {
		return c.JSON(http.StatusServiceUnavailable, msgNoBackend)
	}
	if h.Targets == nil {
		return c.JSON(http.StatusServiceUnavailable, msgNoStore)
	}
	var req model.QueryReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	row, err := h.Catalog.Lookup(monitoring.MetricRow{Name: req.Metric, Schema: req.Schema}.ID())
	if err != nil {
		return c.JSON(http.StatusNotFound, model.SimpleMsg{Message: "Unknown metric: see GET /metrics"})
	}
	if row.Gap != "" {
		return c.JSON(http.StatusUnprocessableEntity, model.SimpleMsg{Message: "Metric is not supported: " + row.Gap})
	}
	q, msg := querySpec(req)
	if msg != "" {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: msg})
	}
	q.Metric = row.ID()
	if msg := checkAttributes(row, q); msg != "" {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: msg})
	}
	if q.Within != nil && q.Within.Label == "" {
		// within names no attribute: the field's dimension is the only one.
		q.Within.Label = row.DimensionName
	}
	q = query.ToSeriesLabels(row, q)

	switch {
	case len(req.TargetIDs) > 0:
		for _, id := range req.TargetIDs {
			t, err := h.Targets.Lookup(ctx, id)
			if errors.Is(err, monitoring.ErrTargetNotFound) {
				return c.JSON(http.StatusNotFound, model.SimpleMsg{Message: "Target is not registered: " + id})
			}
			if err != nil {
				log.Error().Err(err).Str("targetId", id).Msg("Failed to read target")
				return c.JSON(http.StatusInternalServerError, model.SimpleMsg{Message: "Target lookup failed"})
			}
			q.Targets = append(q.Targets, t)
		}
	case req.Selector != nil:
		if q.Targets, err = h.Targets.Select(ctx, req.Selector); err != nil {
			log.Error().Err(err).Msg("Failed to select targets")
			return c.JSON(http.StatusInternalServerError, model.SimpleMsg{Message: "Target selection failed"})
		}
	default:
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "targetIds or selector is required; selector {} means every target"})
	}
	if len(q.Targets) == 0 {
		return c.JSON(http.StatusOK, model.QueryResp{Series: []model.SeriesInfo{}, Targets: map[string]model.TargetInfo{}})
	}

	series, err := h.Backend.Query(ctx, q)
	if err != nil {
		var apiErr *promapi.APIError
		switch {
		case errors.Is(err, query.ErrInvalidQuery):
			log.Warn().Err(err).Str("metric", q.Metric).Msg("Query could not be built")
			return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Query does not fit the metric: check the window, within, groupBy and topK"})
		case errors.As(err, &apiErr) && apiErr.Type == "bad_data":
			log.Warn().Err(err).Str("metric", q.Metric).Msg("Metrics backend refused the query")
			return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Metrics backend refused the query: narrow the range or widen the step"})
		default:
			log.Error().Err(err).Str("metric", q.Metric).Msg("Metrics backend query failed")
			return c.JSON(http.StatusBadGateway, model.SimpleMsg{Message: "Metrics backend query failed"})
		}
	}
	return c.JSON(http.StatusOK, queryResp(row, series, q.Targets))
}

// checkAttributes says what is wrong with the attributes q names, or "".
func checkAttributes(row monitoring.MetricRow, q monitoring.QuerySpec) string {
	if len(q.Where) == 0 && len(q.SplitBy) == 0 {
		return ""
	}
	if row.Attribute == nil {
		return "Metric has no attribute to split by"
	}
	for _, a := range q.SplitBy {
		if a != row.Attribute.Name {
			return "Metric splits by " + row.Attribute.Name + " only"
		}
	}
	for a, v := range q.Where {
		if a != row.Attribute.Name {
			return "Metric splits by " + row.Attribute.Name + " only"
		}
		if !slices.Contains(row.Attribute.Values, v) {
			return a + " takes one of: " + strings.Join(row.Attribute.Values, ", ")
		}
	}
	return ""
}

// querySpec turns a request into a QuerySpec without its targets, or returns
// what is wrong with it.
func querySpec(req model.QueryReq) (monitoring.QuerySpec, string) {
	q := monitoring.QuerySpec{
		Metric: req.Metric, Start: req.Start, End: req.End,
		Across: monitoring.AggFn(req.Across), GroupBy: req.GroupBy, FillZero: req.FillZero,
		Where: req.Where, SplitBy: req.SplitBy,
	}
	if req.Within != "" {
		q.Within = &monitoring.Reduce{Fn: monitoring.AggFn(req.Within)}
	}
	if req.WindowFn != "" || req.WindowRange != "" {
		d, err := time.ParseDuration(req.WindowRange)
		if err != nil || d <= 0 {
			return q, "windowRange must be a positive duration such as 5m"
		}
		q.Window = monitoring.Agg{Fn: monitoring.AggFn(req.WindowFn), Range: d}
	}
	if req.Step != "" {
		d, err := time.ParseDuration(req.Step)
		if err != nil || d <= 0 {
			return q, "step must be a positive duration such as 1m"
		}
		if req.Start.IsZero() || req.End.IsZero() || !req.End.After(req.Start) {
			return q, "a graph needs start before end"
		}
		q.Step = d
	}
	if req.TopK != nil {
		if req.TopK.N <= 0 {
			return q, "topK.n must be positive"
		}
		q.TopK = &monitoring.TopK{N: req.TopK.N, Desc: req.TopK.Desc, PerLabel: req.TopK.PerLabel}
	}
	return q, ""
}

func queryResp(row monitoring.MetricRow, series []monitoring.Series, targets monitoring.TargetSet) model.QueryResp {
	byID := make(map[string]monitoring.Target, len(targets))
	for _, t := range targets {
		byID[t.EntityID] = t
	}
	resp := model.QueryResp{Series: make([]model.SeriesInfo, 0, len(series)), Targets: map[string]model.TargetInfo{}}
	for _, s := range series {
		info := model.SeriesInfo{Labels: query.ToAttributeNames(row, s.Labels), Points: make([]model.PointInfo, 0, len(s.Points))}
		for _, p := range s.Points {
			pt := model.PointInfo{T: p.T}
			if !math.IsNaN(p.V) && !math.IsInf(p.V, 0) {
				v := p.V
				pt.V = &v
			}
			info.Points = append(info.Points, pt)
		}
		resp.Series = append(resp.Series, info)
		if id := s.Labels[monitoring.LabelEntityID]; id != "" {
			if t, ok := byID[id]; ok {
				resp.Targets[id] = targetInfo(t)
			}
		}
	}
	return resp
}
