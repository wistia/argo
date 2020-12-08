package metrics

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/util/workqueue"

	"github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
)

const (
	argoNamespace            = "argo"
	workflowsSubsystem       = "workflows"
	DefaultMetricsServerPort = 9090
	DefaultMetricsServerPath = "/metrics"
)

type ServerConfig struct {
	Enabled      bool
	Path         string
	Port         int
	TTL          time.Duration
	IgnoreErrors bool
}

func (s ServerConfig) SameServerAs(other ServerConfig) bool {
	return s.Port == other.Port && s.Path == other.Path && s.Enabled && other.Enabled
}

type metric struct {
	metric      prometheus.Metric
	lastUpdated time.Time
}

type Metrics struct {
	// Ensures mutual exclusion in workflows map
	mutex           sync.RWMutex
	metricsConfig   ServerConfig
	telemetryConfig ServerConfig

	workflowsProcessed prometheus.Counter
	workflowsByPhase   map[v1alpha1.NodePhase]prometheus.Gauge
	workflows          map[string][]string
	operationDurations prometheus.Histogram
	errors             map[ErrorCause]prometheus.Counter
	customMetrics      map[string]metric
	workqueueMetrics   map[string]prometheus.Metric

	// Used to quickly check if a metric desc is already used by the system
	defaultMetricDescs map[string]bool
	metricNameHelps    map[string]string
	logMetric          *prometheus.CounterVec

	// Custom Wistia metrics
	podDeletionLatency prometheus.Gauge
	podGCAddedToQueue  prometheus.Counter
	podGCRemovedFromQueue prometheus.Counter
	podInformerAddPod prometheus.Counter
	podInformerUpdatePod prometheus.Counter
	podInformerDeletePod prometheus.Counter
	processNextItemDuration prometheus.Gauge
	workflowQueueDepth prometheus.Gauge
	podQueueDepth      prometheus.Gauge
	deadlineExceeded   prometheus.Counter
}

func (m *Metrics) Levels() []log.Level {
	return []log.Level{log.InfoLevel, log.WarnLevel, log.ErrorLevel}
}

func (m *Metrics) Fire(entry *log.Entry) error {
	m.logMetric.WithLabelValues(entry.Level.String()).Inc()
	return nil
}

var _ prometheus.Collector = &Metrics{}

func New(metricsConfig, telemetryConfig ServerConfig) *Metrics {
	metrics := &Metrics{
		metricsConfig:      metricsConfig,
		telemetryConfig:    telemetryConfig,
		workflowsProcessed: newCounter("workflows_processed_count", "Number of workflow updates processed", nil),
		workflowsByPhase:   getWorkflowPhaseGauges(),
		workflows:          make(map[string][]string),
		operationDurations: newHistogram("operation_duration_seconds", "Histogram of durations of operations", nil, []float64{0.1, 0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 1.75, 2.0, 2.5, 3.0}),
		errors:             getErrorCounters(),
		customMetrics:      make(map[string]metric),
		workqueueMetrics:   make(map[string]prometheus.Metric),
		defaultMetricDescs: make(map[string]bool),
		metricNameHelps:    make(map[string]string),
		logMetric: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "log_messages",
			Help: "Total number of log messages.",
		}, []string{"level"}),
		podDeletionLatency: newGauge("wcustom_pod_deletion_latency", "Latency for pod deletion (ms)", nil),
		podGCAddedToQueue: newCounter("wcustom_pod_gc_added_to_queue", "Pod GC requests added to queue", nil),
		podGCRemovedFromQueue: newCounter("wcustom_pod_gc_removed_from_queue", "Pod GC requests removed from queue", nil),
		podInformerAddPod: newCounter("wcustom_pod_informer_add_pod", "Pod informer notified that a pod was added", nil),
		podInformerUpdatePod: newCounter("wcustom_pod_informer_update_pod", "Pod informer notified that a pod was updated", nil),
		podInformerDeletePod: newCounter("wcustom_pod_informer_delete_pod", "Pod informer notified that a pod was deleted", nil),
		processNextItemDuration: newGauge("wcustom_process_next_item_duration", "Latency for processNextItem (ms)", nil),
		workflowQueueDepth: newGauge("wcustom_workflow_queue_depth", "Depth of workflow queue", nil),
		podQueueDepth: newGauge("wcustom_pod_queue_depth", "Depth of pod queue", nil),
		deadlineExceeded: newCounter("wcustom_deadline_exceeded", "Deadline exceeded", nil),
	}

	for _, metric := range metrics.allMetrics() {
		metrics.defaultMetricDescs[metric.Desc().String()] = true
	}

	for _, level := range metrics.Levels() {
		metrics.logMetric.WithLabelValues(level.String())
	}

	log.AddHook(metrics)

	return metrics
}

func (m *Metrics) allMetrics() []prometheus.Metric {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	allMetrics := []prometheus.Metric{
		m.workflowsProcessed,
		m.operationDurations,
		m.podDeletionLatency,
		m.podGCAddedToQueue,
		m.podGCRemovedFromQueue,
		m.podInformerAddPod,
		m.podInformerUpdatePod,
		m.podInformerDeletePod,
		m.processNextItemDuration,
		m.workflowQueueDepth,
		m.podQueueDepth,
		m.deadlineExceeded,
	}
	for _, metric := range m.workflowsByPhase {
		allMetrics = append(allMetrics, metric)
	}
	for _, metric := range m.errors {
		allMetrics = append(allMetrics, metric)
	}
	for _, metric := range m.workqueueMetrics {
		allMetrics = append(allMetrics, metric)
	}
	for _, metric := range m.customMetrics {
		allMetrics = append(allMetrics, metric.metric)
	}
	return allMetrics
}

func (m *Metrics) UpdatePodDeletionLatency(latencyMs int64) {
	m.podDeletionLatency.Set(float64(latencyMs))
}

func (m *Metrics) IncrementPodGCAddedToQueue() {
	m.podGCAddedToQueue.Inc()
}

func (m *Metrics) IncrementPodGCRemovedFromQueue() {
	m.podGCRemovedFromQueue.Inc()
}

func (m *Metrics) IncrementPodInformerAddPod() {
	m.podInformerAddPod.Inc()
}

func (m *Metrics) IncrementPodInformerUpdatePod() {
	m.podInformerUpdatePod.Inc()
}

func (m *Metrics) IncrementPodInformerDeletePod() {
	m.podInformerDeletePod.Inc()
}

func (m *Metrics) UpdateProcessNextItemDuration(latencyMs int64) {
	m.processNextItemDuration.Set(float64(latencyMs))
}

func (m *Metrics) UpdateWorkflowQueueDepth(depth int) {
	m.workflowQueueDepth.Set(float64(depth))
}

func (m *Metrics) UpdatePodQueueDepth(depth int) {
	m.podQueueDepth.Set(float64(depth))
}

func (m *Metrics) IncrementDeadlineExceeded() {
	m.deadlineExceeded.Inc()
}

func (m *Metrics) StopRealtimeMetricsForKey(key string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, exists := m.workflows[key]; !exists {
		return
	}

	realtimeMetrics := m.workflows[key]
	for _, metric := range realtimeMetrics {
		delete(m.customMetrics, metric)
	}

	delete(m.workflows, key)
}

func (m *Metrics) OperationCompleted(durationSeconds float64) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.operationDurations.Observe(durationSeconds)
}

func (m *Metrics) GetCustomMetric(key string) prometheus.Metric {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// It's okay to return nil metrics in this function
	return m.customMetrics[key].metric
}

func (m *Metrics) UpsertCustomMetric(key string, ownerKey string, newMetric prometheus.Metric, realtime bool) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	metricDesc := newMetric.Desc().String()
	if _, inUse := m.defaultMetricDescs[metricDesc]; inUse {
		return fmt.Errorf("metric '%s' is already in use by the system, please use a different name", newMetric.Desc())
	}
	name, help := recoverMetricNameAndHelpFromDesc(metricDesc)
	if existingHelp, inUse := m.metricNameHelps[name]; inUse && help != existingHelp {
		return fmt.Errorf("metric '%s' has help string '%s' but should have '%s' (help strings must be identical for metrics of the same name)", name, help, existingHelp)
	} else {
		m.metricNameHelps[name] = help
	}
	m.customMetrics[key] = metric{metric: newMetric, lastUpdated: time.Now()}

	// If this is a realtime metric, track it
	if realtime {
		m.workflows[ownerKey] = append(m.workflows[ownerKey], key)
	}

	return nil
}

func (m *Metrics) SetWorkflowPhaseGauge(phase v1alpha1.NodePhase, num int) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.workflowsByPhase[phase].Set(float64(num))
}

type ErrorCause string

const (
	ErrorCauseOperationPanic              ErrorCause = "OperationPanic"
	ErrorCauseCronWorkflowSubmissionError ErrorCause = "CronWorkflowSubmissionError"
)

func (m *Metrics) OperationPanic() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.errors[ErrorCauseOperationPanic].Inc()
}

func (m *Metrics) CronWorkflowSubmissionError() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.errors[ErrorCauseCronWorkflowSubmissionError].Inc()
}

// Act as a metrics provider for a workflow queue
var _ workqueue.MetricsProvider = &Metrics{}

func (m *Metrics) NewDepthMetric(name string) workqueue.GaugeMetric {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	key := fmt.Sprintf("%s-depth", name)
	if _, ok := m.workqueueMetrics[key]; !ok {
		m.workqueueMetrics[key] = newGauge("queue_depth_count", "Depth of the queue", map[string]string{"queue_name": name})
	}
	return m.workqueueMetrics[key].(prometheus.Gauge)
}

func (m *Metrics) NewAddsMetric(name string) workqueue.CounterMetric {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	key := fmt.Sprintf("%s-adds", name)
	if _, ok := m.workqueueMetrics[key]; !ok {
		m.workqueueMetrics[key] = newCounter("queue_adds_count", "Adds to the queue", map[string]string{"queue_name": name})
	}
	return m.workqueueMetrics[key].(prometheus.Counter)
}

func (m *Metrics) NewLatencyMetric(name string) workqueue.HistogramMetric {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	key := fmt.Sprintf("%s-latency", name)
	if _, ok := m.workqueueMetrics[key]; !ok {
		m.workqueueMetrics[key] = newHistogram("queue_latency", "Time objects spend waiting in the queue", map[string]string{"queue_name": name}, []float64{1.0, 5.0, 20.0, 60.0, 180.0})
	}
	return m.workqueueMetrics[key].(prometheus.Histogram)
}

// These metrics are not relevant to be exposed
type noopMetric struct{}

func (noopMetric) Inc()            {}
func (noopMetric) Dec()            {}
func (noopMetric) Set(float64)     {}
func (noopMetric) Observe(float64) {}

func (m *Metrics) NewRetriesMetric(name string) workqueue.CounterMetric        { return noopMetric{} }
func (m *Metrics) NewWorkDurationMetric(name string) workqueue.HistogramMetric { return noopMetric{} }
func (m *Metrics) NewUnfinishedWorkSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return noopMetric{}
}
func (m *Metrics) NewLongestRunningProcessorSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return noopMetric{}
}
