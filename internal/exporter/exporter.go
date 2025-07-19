package exporter

import (
	"fmt"
	"strings" // Added for the URL fix
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/showwin/speedtest-go/speedtest"
	log "github.com/sirupsen/logrus"
)

const (
	namespace = "speedtest"
	// Sanity threshold for speed values. If a value is > 20,000,
	// we assume it's an anomaly reported in B/s, not Mbps.
	speedThreshold = 20000.0
)

var (
	up = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "up"),
		"Was the last speedtest successful.",
		[]string{"test_uuid"}, nil,
	)
	scrapeDurationSeconds = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "scrape_duration_seconds"),
		"Time to preform last speed test",
		[]string{"test_uuid"}, nil,
	)
	latency = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "latency_seconds"),
		"Measured latency on last speed test",
		[]string{"test_uuid", "user_lat", "user_lon", "user_ip", "user_isp", "server_lat", "server_lon", "server_id", "server_name", "server_country", "distance"},
		nil,
	)
	upload = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "upload_speed_Bps"),
		"Last upload speedtest result in Bytes per second",
		[]string{"test_uuid", "user_lat", "user_lon", "user_ip", "user_isp", "server_lat", "server_lon", "server_id", "server_name", "server_country", "distance"},
		nil,
	)
	download = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "download_speed_Bps"),
		"Last download speedtest result in Bytes per second",
		[]string{"test_uuid", "user_lat", "user_lon", "user_ip", "user_isp", "server_lat", "server_lon", "server_id", "server_name", "server_country", "distance"},
		nil,
	)
)

// Exporter runs speedtest and exports them using
// the prometheus metrics package.
type Exporter struct {
	serverID       int
	serverFallback bool
}

// New returns an initialized Exporter.
func New(serverID int, serverFallback bool) (*Exporter, error) {
	return &Exporter{
		serverID:       serverID,
		serverFallback: serverFallback,
	}, nil
}

// Describe describes all the metrics. It implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- up
	ch <- scrapeDurationSeconds
	ch <- latency
	ch <- upload
	ch <- download
}

// Collect fetches the stats and delivers them as Prometheus metrics.
// It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	testUUID := uuid.New().String()
	start := time.Now()
	ok := e.speedtest(testUUID, ch)

	// Always report up and scrape_duration, regardless of test success
	duration := time.Since(start).Seconds()
	ch <- prometheus.MustNewConstMetric(scrapeDurationSeconds, prometheus.GaugeValue, duration, testUUID)

	if ok {
		ch <- prometheus.MustNewConstMetric(up, prometheus.GaugeValue, 1.0, testUUID)
	} else {
		ch <- prometheus.MustNewConstMetric(up, prometheus.GaugeValue, 0.0, testUUID)
	}
}

func (e *Exporter) speedtest(testUUID string, ch chan<- prometheus.Metric) bool {
	user, err := speedtest.FetchUserInfo()
	if err != nil {
		log.Errorf("could not fetch user information: %s", err.Error())
		return false
	}

	serverList, err := speedtest.FetchServerList(user)
	if err != nil {
		log.Errorf("could not fetch server list: %s", err.Error())
		return false
	}

	var server *speedtest.Server

	if e.serverID == -1 {
		if len(serverList.Servers) == 0 {
			log.Error("server list is empty, cannot select the closest server")
			return false
		}
		server = serverList.Servers[0]
	} else {
		servers, err := serverList.FindServer([]int{e.serverID})
		if err != nil {
			log.Errorf("failed to find server with ID %d: %v", e.serverID, err)
			return false
		}

		if len(servers) == 0 {
			log.Errorf("could not find your chosen server ID %d in the list of available servers", e.serverID)
			if !e.serverFallback {
				log.Info("server_fallback is not enabled, failing this test")
				return false
			}
			log.Info("server_fallback is enabled, falling back to the closest server")
			if len(serverList.Servers) == 0 {
				log.Error("server list is empty, cannot fall back to the closest server")
				return false
			}
			server = serverList.Servers[0]
		} else {
			server = servers[0]
		}
	}

	// WORKAROUND: Detect and correct malformed URLs (e.g., "http//...")
	// that can be produced by the speedtest-go library or server lists.
	if strings.HasPrefix(server.URL, "http//") {
		correctedURL := strings.Replace(server.URL, "http//", "http://", 1)
		log.Warnf("Malformed server URL detected, correcting from '%s' to '%s'", server.URL, correctedURL)
		server.URL = correctedURL
	}

	log.Infof("Starting speedtest with server %s (%s, %s) [id: %s]", server.Name, server.Country, server.Host, server.ID)

	// Run all tests and report individual success/failure.
	pingSuccess := pingTest(testUUID, user, server, ch)
	downloadSuccess := downloadTest(testUUID, user, server, ch)
	uploadSuccess := uploadTest(testUUID, user, server, ch)

	// The overall test is successful if all parts succeed.
	return pingSuccess && downloadSuccess && uploadSuccess
}

func pingTest(testUUID string, user *speedtest.User, server *speedtest.Server, ch chan<- prometheus.Metric) bool {
	err := server.PingTest()
	if err != nil {
		log.Errorf("failed to carry out ping test: %s", err.Error())
		return false
	}

	ch <- prometheus.MustNewConstMetric(
		latency, prometheus.GaugeValue, server.Latency.Seconds(),
		testUUID, user.Lat, user.Lon, user.IP, user.Isp,
		server.Lat, server.Lon, server.ID, server.Name, server.Country, fmt.Sprintf("%f", server.Distance),
	)
	log.Infof("Ping test successful. Latency: %s", server.Latency)
	return true
}

func downloadTest(testUUID string, user *speedtest.User, server *speedtest.Server, ch chan<- prometheus.Metric) bool {
	err := server.DownloadTest(false)
	if err != nil {
		log.Errorf("failed to carry out download test: %s", err.Error())
		return false
	}

	rawValue := server.DLSpeed
	var speedBps float64

	// Heuristic to handle inconsistent units from different speedtest servers.
	if rawValue > speedThreshold {
		log.Warnf("Anomalously high download speed value detected (%.2f). Assuming unit is Bytes/sec.", rawValue)
		speedBps = rawValue // Assume value is already in Bytes/sec
	} else {
		// Assume value is in Mbps, convert to Bytes/sec (1 Mbps = 125,000 B/s)
		speedBps = rawValue * 125000
	}

	ch <- prometheus.MustNewConstMetric(
		download, prometheus.GaugeValue, speedBps,
		testUUID, user.Lat, user.Lon, user.IP, user.Isp,
		server.Lat, server.Lon, server.ID, server.Name, server.Country, fmt.Sprintf("%f", server.Distance),
	)
	log.Infof("Download test successful. Speed: %.2f B/s (%.2f MB/s)", speedBps, speedBps/1000/1000)
	return true
}

func uploadTest(testUUID string, user *speedtest.User, server *speedtest.Server, ch chan<- prometheus.Metric) bool {
	err := server.UploadTest(false)
	if err != nil {
		log.Errorf("failed to carry out upload test: %s", err.Error())
		return false
	}

	rawValue := server.ULSpeed
	var speedBps float64

	// Heuristic to handle inconsistent units from different speedtest servers.
	if rawValue > speedThreshold {
		log.Warnf("Anomalously high upload speed value detected (%.2f). Assuming unit is Bytes/sec.", rawValue)
		speedBps = rawValue // Assume value is already in Bytes/sec
	} else {
		// Assume value is in Mbps, convert to Bytes/sec (1 Mbps = 125,000 B/s)
		speedBps = rawValue * 125000
	}

	ch <- prometheus.MustNewConstMetric(
		upload, prometheus.GaugeValue, speedBps,
		testUUID, user.Lat, user.Lon, user.IP, user.Isp,
		server.Lat, server.Lon, server.ID, server.Name, server.Country, fmt.Sprintf("%f", server.Distance),
	)
	log.Infof("Upload test successful. Speed: %.2f B/s (%.2f MB/s)", speedBps, speedBps/1000/1000)
	return true
}
