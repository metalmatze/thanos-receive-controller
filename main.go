package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/improbable-eng/thanos/pkg/receive"
	"github.com/oklog/run"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	resyncPeriod                  = 5 * time.Minute
	internalServerShutdownTimeout = time.Second
)

func main() {
	config := struct {
		KubeConfig             string
		Namespace              string
		StatefulSetLabel       string
		ClusterDomain          string
		ConfigMapName          string
		ConfigMapGeneratedName string
		FileName               string
		Path                   string
		Port                   int
		Scheme                 string
		InternalAddr           string
	}{}

	flag.StringVar(&config.KubeConfig, "kubeconfig", "", "Path to kubeconfig")
	flag.StringVar(&config.Namespace, "namespace", "default", "The namespace to watch")
	flag.StringVar(&config.StatefulSetLabel, "statefulset-label", "controller.receive.thanos.io=thanos-receive-controller", "The label StatefulSets must have to be watched by the controller")
	flag.StringVar(&config.ClusterDomain, "cluster-domain", "cluster.local", "The DNS domain of the cluster")
	flag.StringVar(&config.ConfigMapName, "configmap-name", "", "The name of the original ConfigMap containing the hashring tenant configuration")
	flag.StringVar(&config.ConfigMapGeneratedName, "configmap-generated-name", "", "The name of the generated and populated ConfigMap")
	flag.StringVar(&config.FileName, "file-name", "", "The name of the configuration file in the ConfigMap")
	flag.StringVar(&config.Path, "path", "/api/v1/receive", "The URL path on which receive components accept write requests")
	flag.IntVar(&config.Port, "port", 19291, "The port on which receive components are listening for write requests")
	flag.StringVar(&config.Scheme, "scheme", "http", "The URL scheme on which receive components accept write requests")
	flag.StringVar(&config.InternalAddr, "internal-addr", ":8080", "The address on which internal server runs")
	flag.Parse()

	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.WithPrefix(logger, "ts", log.DefaultTimestampUTC)
	logger = log.WithPrefix(logger, "caller", log.DefaultCaller)

	parts := strings.Split(config.StatefulSetLabel, "=")
	if len(parts) != 2 {
		stdlog.Fatal("--statefulset-label must be of the form 'key=value'")
	}
	labelKey, labelValue := parts[0], parts[1]

	konfig, err := clientcmd.BuildConfigFromFlags("", config.KubeConfig)
	if err != nil {
		stdlog.Fatal(err)
	}

	klient, err := kubernetes.NewForConfig(konfig)
	if err != nil {
		stdlog.Fatal(err)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		version.NewCollector("thanos-receive-controller"),
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	cache.SetReflectorMetricsProvider(newReflactorMetrics(reg))

	var g run.Group
	{
		// Signal chans must be buffered.
		sig := make(chan os.Signal, 1)
		g.Add(func() error {
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			return nil
		}, func(_ error) {
			level.Info(logger).Log("msg", "caught interrrupt")
			close(sig)
		})
	}
	{
		opt := &options{
			clusterDomain:          config.ClusterDomain,
			configMapName:          config.ConfigMapName,
			configMapGeneratedName: config.ConfigMapGeneratedName,
			fileName:               config.FileName,
			namespace:              config.Namespace,
			path:                   config.Path,
			port:                   config.Port,
			scheme:                 config.Scheme,
			labelKey:               labelKey,
			labelValue:             labelValue,
		}
		c := newController(klient, logger, opt)
		c.registerMetrics(reg)
		done := make(chan struct{})

		g.Add(func() error {
			return c.run(done)
		}, func(_ error) {
			level.Info(logger).Log("msg", "shutting down controller")
			close(done)
		})
	}
	{
		router := http.NewServeMux()
		router.Handle("/metrics", promhttp.InstrumentMetricHandler(reg, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})))
		srv := &http.Server{Addr: config.InternalAddr, Handler: router}

		g.Add(func() error {
			return srv.ListenAndServe()
		}, func(err error) {
			if err != http.ErrServerClosed {
				level.Warn(logger).Log("msg", "internal server closed unexpectedly")
				return
			}
			level.Info(logger).Log("msg", "shutting down internal server")
			ctx, _ := context.WithTimeout(context.Background(), internalServerShutdownTimeout)
			if err := srv.Shutdown(ctx); err != nil {
				stdlog.Fatal(err)
			}
		})
	}

	level.Info(logger).Log("msg", "starting the controller")
	if err := g.Run(); err != nil {
		stdlog.Fatal(err)
	}
}

type prometheusReflectorMetrics struct {
	reg *prometheus.Registry
}

func newReflactorMetrics(reg *prometheus.Registry) prometheusReflectorMetrics {
	return prometheusReflectorMetrics{reg}
}

func (p prometheusReflectorMetrics) counter(name string) prometheus.Counter {
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "thanos-receive-controller",
		Name:      name,
	})
	p.reg.MustRegister(counter)
	return counter
}

func (p prometheusReflectorMetrics) summary(name string) prometheus.Summary {
	summary := prometheus.NewSummary(prometheus.SummaryOpts{
		Subsystem: "thanos-receive-controller",
		Name:      name,
	})
	p.reg.MustRegister(summary)
	return summary
}

func (p prometheusReflectorMetrics) gauge(name string) prometheus.Gauge {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "thanos-receive-controller",
		Name:      name,
	})
	p.reg.MustRegister(gauge)
	return gauge
}

func (p prometheusReflectorMetrics) NewListsMetric(name string) cache.CounterMetric {
	return p.counter(name)
}

func (p prometheusReflectorMetrics) NewListDurationMetric(name string) cache.SummaryMetric {
	return p.summary(name)
}

func (p prometheusReflectorMetrics) NewItemsInListMetric(name string) cache.SummaryMetric {
	return p.summary(name)
}

func (p prometheusReflectorMetrics) NewWatchesMetric(name string) cache.CounterMetric {
	return p.counter(name)
}

func (p prometheusReflectorMetrics) NewShortWatchesMetric(name string) cache.CounterMetric {
	return p.counter(name)
}

func (p prometheusReflectorMetrics) NewWatchDurationMetric(name string) cache.SummaryMetric {
	return p.summary(name)
}

func (p prometheusReflectorMetrics) NewItemsInWatchMetric(name string) cache.SummaryMetric {
	return p.summary(name)
}

func (p prometheusReflectorMetrics) NewLastResourceVersionMetric(name string) cache.GaugeMetric {
	return p.gauge(name)
}

type options struct {
	clusterDomain          string
	configMapName          string
	configMapGeneratedName string
	fileName               string
	namespace              string
	path                   string
	port                   int
	scheme                 string
	labelKey               string
	labelValue             string
}

type controller struct {
	options *options
	queue   *queue
	logger  log.Logger

	klient  kubernetes.Interface
	cmapInf cache.SharedIndexInformer
	ssetInf cache.SharedIndexInformer

	reconcileAttempts       prometheus.Counter
	reconcileErrors         *prometheus.CounterVec
	configmapChangeAttempts *prometheus.CounterVec
	configmapChangeErrors   prometheus.Counter
}

func newController(klient kubernetes.Interface, logger log.Logger, o *options) *controller {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	return &controller{
		options: o,
		queue:   newQueue(),
		logger:  logger,

		klient:  klient,
		cmapInf: coreinformers.NewConfigMapInformer(klient, o.namespace, resyncPeriod, nil),
		ssetInf: appsinformers.NewFilteredStatefulSetInformer(klient, o.namespace, resyncPeriod, nil, func(lo *v1.ListOptions) {
			lo.LabelSelector = labels.Set{o.labelKey: o.labelValue}.String()
		}),

		reconcileAttempts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "thanos_receive_controller_reconcile_attempts_total",
			Help: "Total number of reconciles.",
		}),

		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "thanos_receive_controller_reconcile_errors_total",
			Help: "Total number of reconciles errors.",
		},
			[]string{"type"},
		),

		configmapChangeAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "thanos_receive_controller_configmap_change_attempts_total",
			Help: "Total number of configmap change attempts.",
		},
			[]string{"verb"},
		),

		configmapChangeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "thanos_receive_controller_configmap_change_errors_total",
			Help: "Total number of configmap change errors.",
		}),
	}
}

func (c *controller) registerMetrics(reg *prometheus.Registry) {
	if reg != nil {
		c.reconcileAttempts.Add(0)
		c.reconcileErrors.WithLabelValues().Add(0)
		c.configmapChangeAttempts.WithLabelValues().Add(0)
		c.configmapChangeErrors.Add(0)
		reg.MustRegister(
			c.reconcileAttempts,
			c.reconcileErrors,
			c.configmapChangeAttempts,
			c.configmapChangeErrors,
		)
	}
}

func (c *controller) run(stop <-chan struct{}) error {
	defer c.queue.stop()

	go c.cmapInf.Run(stop)
	go c.ssetInf.Run(stop)
	if err := c.waitForCacheSync(stop); err != nil {
		return err
	}
	// TODO: Collect global informer metrics
	c.cmapInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { c.queue.add() },
		DeleteFunc: func(_ interface{}) { c.queue.add() },
		UpdateFunc: func(_, _ interface{}) { c.queue.add() },
	})
	c.ssetInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { c.queue.add() },
		DeleteFunc: func(_ interface{}) { c.queue.add() },
		UpdateFunc: func(_, _ interface{}) { c.queue.add() },
	})
	go c.worker()

	<-stop
	return nil
}

// waitForCacheSync waits for the informers' caches to be synced.
func (c *controller) waitForCacheSync(stop <-chan struct{}) error {
	ok := true
	informers := []struct {
		name     string
		informer cache.SharedIndexInformer
	}{
		{"ConfigMap", c.cmapInf},
		{"StatefulSet", c.ssetInf},
	}
	for _, inf := range informers {
		if !cache.WaitForCacheSync(stop, inf.informer.HasSynced) {
			level.Error(c.logger).Log("msg", fmt.Sprintf("failed to sync %s cache", inf.name))
			ok = false
		} else {
			level.Debug(c.logger).Log("msg", fmt.Sprintf("successfully synced %s cache", inf.name))
		}
	}
	if !ok {
		return errors.New("failed to sync caches")
	}
	level.Info(c.logger).Log("msg", "successfully synced all caches")
	return nil
}

func (c *controller) worker() {
	for c.queue.get() {
		c.sync()
	}
}

func (c *controller) sync() {
	c.reconcileAttempts.Inc()
	configMap, ok, err := c.cmapInf.GetStore().GetByKey(fmt.Sprintf("%s/%s", c.options.namespace, c.options.configMapName))
	if !ok || err != nil {
		c.reconcileErrors.With(prometheus.Labels{"type": "fetch"}).Inc()
		level.Warn(c.logger).Log("msg", "could not fetch ConfigMap", "err", err, "name", c.options.configMapName)
		return
	}
	cm := configMap.(*corev1.ConfigMap)

	var hashrings []receive.HashringConfig
	if err := json.Unmarshal([]byte(cm.Data[c.options.fileName]), &hashrings); err != nil {
		c.reconcileErrors.With(prometheus.Labels{"type": "decode"}).Inc()
		level.Warn(c.logger).Log("msg", "failed to decode configuration", "err", err)
		return
	}

	statefulsets := make(map[string]*appsv1.StatefulSet)
	for _, obj := range c.ssetInf.GetStore().List() {
		sts := obj.(*appsv1.StatefulSet)
		statefulsets[sts.Name] = sts
	}

	c.populate(hashrings, statefulsets)
	if err := c.saveHashring(hashrings); err != nil {
		c.reconcileErrors.With(prometheus.Labels{"type": "save"}).Inc()
		c.configmapChangeErrors.Inc()
		level.Error(c.logger).Log("msg", "failed to save hashrings")
	}
}

func (c *controller) populate(hashrings []receive.HashringConfig, statefulsets map[string]*appsv1.StatefulSet) {
	for i, h := range hashrings {
		if sts, exists := statefulsets[h.Hashring]; exists {
			var endpoints []string
			for i := 0; i < int(*sts.Spec.Replicas); i++ {
				endpoints = append(endpoints,
					// TODO: Make sure this is actually correct
					fmt.Sprintf("%s://%s-%d.%s.%s.svc.%s:%d/%s",
						c.options.scheme,
						sts.Name,
						i,
						sts.Spec.ServiceName,
						c.options.namespace,
						c.options.clusterDomain,
						c.options.port,
						strings.TrimPrefix(c.options.path, "/")),
				)
			}
			hashrings[i].Endpoints = endpoints
		}
	}
}

func (c *controller) saveHashring(hashring []receive.HashringConfig) error {
	buf, err := json.Marshal(hashring)
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.options.configMapGeneratedName,
			Namespace: c.options.namespace,
		},
		Data: map[string]string{
			c.options.fileName: string(buf),
		},
		BinaryData: nil,
	}

	_, err = c.klient.CoreV1().ConfigMaps(c.options.namespace).Get(c.options.configMapGeneratedName, metav1.GetOptions{})
	c.configmapChangeAttempts.With(prometheus.Labels{"verb": "GET"}).Inc()
	if kerrors.IsNotFound(err) {
		_, err = c.klient.CoreV1().ConfigMaps(c.options.namespace).Create(cm)
		c.configmapChangeAttempts.With(prometheus.Labels{"verb": "CREATE"}).Inc()
		return err
	}
	if err != nil {
		return err
	}

	_, err = c.klient.CoreV1().ConfigMaps(c.options.namespace).Update(cm)
	c.configmapChangeAttempts.With(prometheus.Labels{"verb": "UPDATE"}).Inc()
	return err
}

// queue is a non-blocking queue.
type queue struct {
	sync.Mutex
	ch chan struct{}
	ok bool
}

func newQueue() *queue {
	// We want a buffer of size 1 to queue updates
	// while a dequeuer is busy.
	return &queue{ch: make(chan struct{}, 1), ok: true}
}

func (q *queue) add() {
	q.Lock()
	defer q.Unlock()
	if !q.ok {
		return
	}
	select {
	case q.ch <- struct{}{}:
	default:
	}
}

func (q *queue) stop() {
	q.Lock()
	defer q.Unlock()
	if !q.ok {
		return
	}
	close(q.ch)
	q.ok = false
}

func (q *queue) get() bool {
	<-q.ch
	q.Lock()
	defer q.Unlock()
	return q.ok
}
