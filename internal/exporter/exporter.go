package exporter

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/showwin/speedtest-go/speedtest"
	log "github.com/sirupsen/logrus"
)

const (
	namespace = "speedtest"
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
		"Last upload speedtest result",
		[]string{"test_uuid", "user_lat", "user_lon", "user_ip", "user_isp", "server_lat", "server_lon", "server_id", "server_name", "server_country", "distance"},
		nil,
	)
	download = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "download_speed_Bps"),
		"Last download speedtest result",
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

// Collect fetches the stats from Starlink dish and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	testUUID := uuid.New().String()
	start := time.Now()
	ok := e.speedtest(testUUID, ch)

	if ok {
		ch <- prometheus.MustNewConstMetric(
			up, prometheus.GaugeValue, 1.0,
			testUUID,
		)
		ch <- prometheus.MustNewConstMetric(
			scrapeDurationSeconds, prometheus.GaugeValue, time.Since(start).Seconds(),
			testUUID,
		)
	} else {
		ch <- prometheus.MustNewConstMetric(
			up, prometheus.GaugeValue, 0.0,
			testUUID,
		)
	}
}

func (e *Exporter) speedtest(testUUID string, ch chan<- prometheus.Metric) bool {
	user, err := speedtest.FetchUserInfo()
	if err != nil {
		log.Errorf("could not fetch user information: %s", err.Error())
		return false
	}

	// returns list of servers in distance order
	serverList, err := speedtest.FetchServerList(user)
	if err != nil {
		log.Errorf("could not fetch server list: %s", err.Error())
		return false
	}

	var server *speedtest.Server

	if e.serverID == -1 {
		server = serverList.Servers[0]
	} else {
		servers, err := serverList.FindServer([]int{e.serverID})
		if err != nil {
			log.Error(err)
			return false
		}

		if servers[0].ID != fmt.Sprintf("%d", e.serverID) && !e.serverFallback {
			log.Errorf("could not find your choosen server ID %d in the list of avaiable servers, server_fallback is not set so failing this test", e.serverID)
			return false
		}

		server = servers[0]
	}

	ok := pingTest(testUUID, user, server, ch)
	ok = downloadTest(testUUID, user, server, ch) && ok
	ok = uploadTest(testUUID, user, server, ch) && ok

	return ok
}

func pingTest(testUUID string, user *speedtest.User, server *speedtest.Server, ch chan<- prometheus.Metric) bool {
	err := server.PingTest()
	if err != nil {
		log.Errorf("failed to carry out ping test: %s", err.Error())
		return false
	}

	ch <- prometheus.MustNewConstMetric(
		latency, prometheus.GaugeValue, server.Latency.Seconds(),
		testUUID,
		user.Lat,
		user.Lon,
		user.IP,
		user.Isp,
		server.Lat,
		server.Lon,
		server.ID,
		server.Name,
		server.Country,
		fmt.Sprintf("%f", server.Distance),
	)

	return true
}

func downloadTest(testUUID string, user *speedtest.User, server *speedtest.Server, ch chan<- prometheus.Metric) bool {
	err := server.DownloadTest(false)
	if err != nil {
		log.Errorf("failed to carry out download test: %s", err.Error())
		return false
	}

	// Extract the raw numeric value from the speedtest result
	rawValue := float64(server.DLSpeed)
	var speedBps float64
	// due to some servers running older versions of speedtest they report diffrent metrics in ether bps or mbps covert if over value
	// if the raw value is very large (> 20,000), assumein Bytes/sec
	// otherwise, we assume it's in Megabits/sec (Mbps) and convert it
	// change value for sanity check assuming here noone has more than 26gbit lines and allows for minimum speeds to be reported in bytes of 20kb/s to be reported
	if rawValue > 26000 {
		log.Warnf("Anomalously high speed value detected (%.2f). Assuming unit is Bytes/sec.", rawValue)
		speedBps = rawValue
	} else {
		// if the value is in a normal range for Mbps, convert it to Bytes/sec.
		// 1 Mbps = 125,000 B/s
		// else ignore
		speedBps = rawValue * 125000
	}

	ch <- prometheus.MustNewConstMetric(
		download, prometheus.GaugeValue, speedBps,
		testUUID,
		user.Lat,
		user.Lon,
		user.IP,
		user.Isp,
		server.Lat,
		server.Lon,
		server.ID,
		server.Name,
		server.Country,
		fmt.Sprintf("%f", server.Distance),
	)

	return true
}

func uploadTest(testUUID string, user *speedtest.User, server *speedtest.Server, ch chan<- prometheus.Metric) bool {
	err := server.UploadTest(false)
	if err != nil {
		log.Errorf("failed to carry out upload test: %s", err.Error())
		return false
	}

	// Extract the raw numeric value from the speedtest result
	rawValue := float64(server.ULSpeed)
	var speedBps float64

	// Heuristic to handle inconsistent units from different speedtest servers.
	if rawValue > 26000 {
		log.Warnf("Anomalously high speed value detected (%.2f). Assuming unit is Bytes/sec.", rawValue)
		speedBps = rawValue
	} else {
		// If the value is in a normal range for Mbps, convert it to Bytes/sec.
		speedBps = rawValue * 125000
	}

	ch <- prometheus.MustNewConstMetric(
		upload, prometheus.GaugeValue, speedBps,
		testUUID,
		user.Lat,
		user.Lon,
		user.IP,
		user.Isp,
		server.Lat,
		server.Lon,
		server.ID,
		server.Name,
		server.Country,
		fmt.Sprintf("%f", server.Distance),
	)

	return true
}
