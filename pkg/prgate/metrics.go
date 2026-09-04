package prgate

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"
)

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsCollector collects and exposes PR validation metrics
type MetricsCollector struct {
	// Validation metrics
	validationsTotal      *prometheus.CounterVec
	validationsDuration   *prometheus.HistogramVec
	validationsErrors     *prometheus.CounterVec
	
	// Check-specific metrics
	checksTotal          *prometheus.CounterVec
	checksDuration       *prometheus.HistogramVec
	checksErrors         *prometheus.CounterVec
	
	// PR metadata metrics (no per-author split: a GitHub username is a
	// per-entity identifier and never a metric label — obs-plan §1.2/§5.6)
	prsActive            prometheus.Gauge
	prsBySize            *prometheus.GaugeVec
	prsByCheckStatus     *prometheus.GaugeVec
	
	// System metrics
	systemErrors         *prometheus.CounterVec
	apiCallsTotal        *prometheus.CounterVec
	apiCallsDuration     *prometheus.HistogramVec
	
	mu                   sync.RWMutex
	registry             prometheus.Registerer
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector(registry prometheus.Registerer) *MetricsCollector {
	if registry == nil {
		registry = prometheus.DefaultRegisterer
	}
	
	collector := &MetricsCollector{
		registry: registry,
	}
	
	// Initialize metrics
	collector.initializeMetrics()
	
	// Register metrics
	if err := registry.Register(collector); err != nil {
		// Don't fail if metrics are already registered
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			fmt.Printf("Failed to register metrics: %v\n", err)
		}
	}
	
	return collector
}

// initializeMetrics initializes all metrics
func (m *MetricsCollector) initializeMetrics() {
	// Validation metrics
	m.validationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pr_validation_total",
			Help: "Total number of PR validation operations",
		},
		[]string{"outcome"},
	)
	
	m.validationsDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pr_validation_duration_seconds",
			Help:    "Duration of PR validation operations",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"outcome"},
	)
	
	m.validationsErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pr_validation_errors_total",
			Help: "Total number of PR validation errors",
		},
		[]string{"error_code"},
	)
	
	// Check-specific metrics
	m.checksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pr_check_total",
			Help: "Total number of individual check executions",
		},
		[]string{"check", "outcome"},
	)
	
	m.checksDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pr_check_duration_seconds",
			Help:    "Duration of individual check executions",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"check"},
	)
	
	m.checksErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pr_check_errors_total",
			Help: "Total number of individual check errors",
		},
		[]string{"check", "error_code"},
	)
	
	// PR metadata metrics
	m.prsActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "pr_validation_active",
			Help: "Number of currently active PR validations",
		},
	)
	
	m.prsBySize = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pr_validation_active_by_size",
			Help: "Number of currently active PR validations by size category",
		},
		[]string{"kind"},
	)
	
	m.prsByCheckStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pr_validation_active_by_check_status",
			Help: "Number of currently active PR validations by check status",
		},
		[]string{"outcome"},
	)
	
	// System metrics
	m.systemErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pr_system_errors_total",
			Help: "Total number of system-level errors",
		},
		[]string{"error_code"},
	)
	
	m.apiCallsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pr_api_calls_total",
			Help: "Total number of API calls",
		},
		[]string{"operation", "outcome"},
	)
	
	m.apiCallsDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pr_api_call_duration_seconds",
			Help:    "Duration of API calls",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)
}

// RecordValidation records a validation operation
func (m *MetricsCollector) RecordValidation(ctx context.Context, prNumber int, author string, status string, duration time.Duration) {
	m.validationsTotal.WithLabelValues(status).Inc()
	m.validationsDuration.WithLabelValues(status).Observe(duration.Seconds())
}

// RecordValidationError records a validation error
func (m *MetricsCollector) RecordValidationError(ctx context.Context, prNumber int, errorType string) {
	m.validationsErrors.WithLabelValues(errorType).Inc()
}

// RecordCheck records an individual check execution
func (m *MetricsCollector) RecordCheck(ctx context.Context, prNumber int, checkName string, status string, duration time.Duration) {
	m.checksTotal.WithLabelValues(checkName, status).Inc()
	m.checksDuration.WithLabelValues(checkName).Observe(duration.Seconds())
}

// RecordCheckError records an individual check error
func (m *MetricsCollector) RecordCheckError(ctx context.Context, prNumber int, checkName string, errorType string) {
	m.checksErrors.WithLabelValues(checkName, errorType).Inc()
}

// RecordAPICall records an API call
func (m *MetricsCollector) RecordAPICall(ctx context.Context, prNumber int, apiType string, status string, duration time.Duration) {
	m.apiCallsTotal.WithLabelValues(apiType, status).Inc()
	m.apiCallsDuration.WithLabelValues(apiType).Observe(duration.Seconds())
}

// RecordSystemError records a system-level error
func (m *MetricsCollector) RecordSystemError(ctx context.Context, errorType string) {
	m.systemErrors.WithLabelValues(errorType).Inc()
}

// UpdateActivePRs updates PR metadata metrics
func (m *MetricsCollector) UpdateActivePRs(prs []PullRequestMetadata) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	// Reset gauges
	m.prsBySize.Reset()
	m.prsByCheckStatus.Reset()
	
	// Update metrics
	m.prsActive.Set(float64(len(prs)))
	for _, pr := range prs {
		m.prsBySize.WithLabelValues(pr.SizeCategory).Inc()
		m.prsByCheckStatus.WithLabelValues(pr.CheckStatus).Inc()
	}
}

// GetSystemMetrics returns current system metrics
func (m *MetricsCollector) GetSystemMetrics() SystemMetrics {
	return SystemMetrics{
		Goroutines:      runtime.NumGoroutine(),
		MemoryUsage:     getMemoryUsage(),
		CPUUsage:        getCPUUsage(),
		ActiveValidations: m.getActiveValidations(),
	}
}

// PullRequestMetadata contains metadata for PR metrics
type PullRequestMetadata struct {
	PRNumber     int
	Author       string
	SizeCategory string    // "small", "medium", "large"
	CheckStatus  string    // "passing", "failing", "warning"
	CreatedAt    time.Time
}

// SystemMetrics contains system-level metrics
type SystemMetrics struct {
	Goroutines      int
	MemoryUsage     float64 // MB
	CPUUsage        float64 // %
	ActiveValidations int
}

// Prometheus metrics implementation
func (m *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	m.validationsTotal.Describe(ch)
	m.validationsDuration.Describe(ch)
	m.validationsErrors.Describe(ch)
	m.checksTotal.Describe(ch)
	m.checksDuration.Describe(ch)
	m.checksErrors.Describe(ch)
	m.prsBySize.Describe(ch)
	m.prsByCheckStatus.Describe(ch)
	m.systemErrors.Describe(ch)
	m.apiCallsTotal.Describe(ch)
	m.apiCallsDuration.Describe(ch)
}

func (m *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	m.validationsTotal.Collect(ch)
	m.validationsDuration.Collect(ch)
	m.validationsErrors.Collect(ch)
	m.checksTotal.Collect(ch)
	m.checksDuration.Collect(ch)
	m.checksErrors.Collect(ch)
	m.prsBySize.Collect(ch)
	m.prsByCheckStatus.Collect(ch)
	m.systemErrors.Collect(ch)
	m.apiCallsTotal.Collect(ch)
	m.apiCallsDuration.Collect(ch)
}

// getMemoryUsage returns current memory usage in MB
func getMemoryUsage() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.Alloc) / 1024 / 1024
}

// getCPUUsage returns current CPU usage percentage (mock implementation)
func getCPUUsage() float64 {
	// In a real implementation, this would use actual CPU metrics
	// For now, return a mock value
	return 25.5
}

// getActiveValidations returns number of active validations (mock implementation)
func (m *MetricsCollector) getActiveValidations() int {
	// In a real implementation, this would track active validations
	// For now, return a mock value
	return 0
}

// PerformanceMonitor wraps the PR validation gate with performance monitoring
type PerformanceMonitor struct {
	gate        *PRValidationGate
	metrics     *MetricsCollector
	enabled     bool
}

// NewPerformanceMonitor creates a new performance monitor
func NewPerformanceMonitor(gate *PRValidationGate, metrics *MetricsCollector, enabled bool) *PerformanceMonitor {
	return &PerformanceMonitor{
		gate:    gate,
		metrics: metrics,
		enabled: enabled,
	}
}

// Validate wraps the Validate method with performance monitoring
func (m *PerformanceMonitor) Validate(ctx context.Context, prData *PullRequestData) (*ValidationResult, error) {
	if !m.enabled {
		return m.gate.Validate(ctx, prData)
	}
	
	startTime := time.Now()
	author := prData.Author
	prNumber := prData.PRNumber
	
	// Record validation start
	m.metrics.RecordAPICall(ctx, prNumber, "validation_start", "success", 0)
	
	// Execute validation
	result, err := m.gate.Validate(ctx, prData)
	
	duration := time.Since(startTime)
	
	if err != nil {
		// Record validation failure
		m.metrics.RecordValidation(ctx, prNumber, author, "error", duration)
		m.metrics.RecordValidationError(ctx, prNumber, "validation_error")
		
		// Record individual check errors
		for _, check := range result.CheckResults {
			if check.Status == "fail" {
				m.metrics.RecordCheckError(ctx, prNumber, check.Name, "check_failure")
			}
		}
	} else {
		// Record validation success or failure
		status := "success"
		if !result.IsValid {
			status = "fail"
		}
		m.metrics.RecordValidation(ctx, prNumber, author, status, duration)
	}
	
	// Record individual check metrics
	for _, check := range result.CheckResults {
		checkStatus := check.Status
		if checkStatus == "warning" && m.gate.config.AllowWarnings {
			checkStatus = "success"
		}
		m.metrics.RecordCheck(ctx, prNumber, check.Name, checkStatus, check.Duration)
	}
	
	// Record system metrics if needed
	if err != nil {
		m.metrics.RecordSystemError(ctx, "validation_failure")
	}
	
	return result, err
}

// GetSystemMetrics returns current system metrics
func (m *PerformanceMonitor) GetSystemMetrics() SystemMetrics {
	return m.metrics.GetSystemMetrics()
}

// GetMetricsCollector returns the underlying metrics collector
func (m *PerformanceMonitor) GetMetricsCollector() *MetricsCollector {
	return m.metrics
}

// StartMetricsServer starts a metrics server for Prometheus
func StartMetricsServer(port int, registry prometheus.Registerer) error {
	http.Handle("/metrics", promhttp.Handler())
	
	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Starting metrics server on %s\n", addr)
	
	return http.ListenAndServe(addr, nil)
}