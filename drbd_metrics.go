package main

import (
	"encoding/json"
	"io/ioutil"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// drbdStatus is for parsing relevant data we want to convert to metrics
type drbdStatus struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Devices []struct {
		Volume    int    `json:"volume"`
		Written   int    `json:"written"`
		Read      int    `json:"read"`
		AlWrites  int    `json:"al-writes"`
		BmWrites  int    `json:"bm-writes"`
		UpPending int    `json:"upper-pending"`
		LoPending int    `json:"lower-pending"`
		Quorum    bool   `json:"quorum"`
		DiskState string `json:"disk-state"`
	} `json:"devices"`
	Connections []struct {
		PeerNodeID  int    `json:"peer-node-id"`
		PeerRole    string `json:"peer-role"`
		PeerDevices []struct {
			Volume        int     `json:"volume"`
			Received      int     `json:"received"`
			Sent          int     `json:"sent"`
			Pending       int     `json:"pending"`
			Unacked       int     `json:"unacked"`
			PeerDiskState string  `json:"peer-disk-state"`
			PercentInSync float64 `json:"percent-in-sync"`
		} `json:"peer_devices"`
	} `json:"connections"`
}

func NewDrbdCollector(drbdSetupPath string, drbdSplitBrainPath string) (*drbdCollector, error) {
	err := CheckExecutables(drbdSetupPath)
	if err != nil {
		return nil, errors.Wrap(err, "could not initialize DRBD collector")
	}

	collector := &drbdCollector{
		DefaultCollector{
			subsystem: "drbd",
		},
		drbdSetupPath,
		drbdSplitBrainPath,
	}

	collector.setDescriptor("resources", "The DRBD resources; 1 line per name, per volume", []string{"resource", "role", "volume", "disk_state"})
	collector.setDescriptor("written", "KiB written to DRBD; 1 line per res, per volume", []string{"resource", "volume"})
	collector.setDescriptor("read", "KiB read from DRBD; 1 line per res, per volume", []string{"resource", "volume"})
	collector.setDescriptor("al_writes", "Writes to activity log; 1 line per res, per volume", []string{"resource", "volume"})
	collector.setDescriptor("bm_writes", "Writes to bitmap; 1 line per res, per volume", []string{"resource", "volume"})
	collector.setDescriptor("upper_pending", "Upper pending; 1 line per res, per volume", []string{"resource", "volume"})
	collector.setDescriptor("lower_pending", "Lower pending; 1 line per res, per volume", []string{"resource", "volume"})
	collector.setDescriptor("quorum", "Quorum status per resource and per volume", []string{"resource", "volume"})
	collector.setDescriptor("connections", "The DRBD resource connections; 1 line per per resource, per peer_node_id", []string{"resource", "peer_node_id", "peer_role", "volume", "peer_disk_state"})
	collector.setDescriptor("connections_sync", "The in sync percentage value for DRBD resource connections", []string{"resource", "peer_node_id", "volume"})
	collector.setDescriptor("connections_received", "KiB received per connection", []string{"resource", "peer_node_id", "volume"})
	collector.setDescriptor("connections_sent", "KiB sent per connection", []string{"resource", "peer_node_id", "volume"})
	collector.setDescriptor("connections_pending", "Pending value per connection", []string{"resource", "peer_node_id", "volume"})
	collector.setDescriptor("connections_unacked", "Unacked value per connection", []string{"resource", "peer_node_id", "volume"})
	collector.setDescriptor("split_brain", "Whether a split brain has been detected; 1 line per resource, per volume.", []string{"resource", "volume"})

	return collector, nil
}

type drbdCollector struct {
	DefaultCollector
	drbdsetupPath      string
	drbdSplitBrainPath string
}

func (c *drbdCollector) Collect(ch chan<- prometheus.Metric) {
	log.Infoln("Collecting DRBD metrics...")

	c.recordDrbdSplitBrainMetric(ch)

	drbdStatusRaw, err := exec.Command(c.drbdsetupPath, "status", "--json").Output()
	if err != nil {
		log.Warnf("Error while retrieving drbd infos %s", err)
		return
	}
	// populate structs and parse relevant info we will expose via metrics
	drbdDev, err := parseDrbdStatus(drbdStatusRaw)
	if err != nil {
		log.Warnf("Error while parsing drbd json: %s", err)
		return
	}

	for _, resource := range drbdDev {
		for _, device := range resource.Devices {
			// the `resources` metric value is always 1, otherwise it's absent
			ch <- c.makeGaugeMetric("resources", float64(1), resource.Name, resource.Role, strconv.Itoa(device.Volume), strings.ToLower(device.DiskState))

			ch <- c.makeGaugeMetric("written", float64(device.Written), resource.Name, strconv.Itoa(device.Volume))

			ch <- c.makeGaugeMetric("read", float64(device.Read), resource.Name, strconv.Itoa(device.Volume))

			ch <- c.makeGaugeMetric("al_writes", float64(device.AlWrites), resource.Name, strconv.Itoa(device.Volume))

			ch <- c.makeGaugeMetric("bm_writes", float64(device.BmWrites), resource.Name, strconv.Itoa(device.Volume))

			ch <- c.makeGaugeMetric("upper_pending", float64(device.UpPending), resource.Name, strconv.Itoa(device.Volume))

			ch <- c.makeGaugeMetric("lower_pending", float64(device.LoPending), resource.Name, strconv.Itoa(device.Volume))

			if bool(device.Quorum) == true {
				ch <- c.makeGaugeMetric("quorum", float64(1), resource.Name, strconv.Itoa(device.Volume))
			} else {
				ch <- c.makeGaugeMetric("quorum", float64(0), resource.Name, strconv.Itoa(device.Volume))
			}
		}
		if len(resource.Connections) == 0 {
			log.Warnf("Could not retrieve connection info for resource '%s'\n", resource.Name)
			continue
		}
		// a Resource can have multiple connection with different nodes
		for _, conn := range resource.Connections {
			if len(conn.PeerDevices) == 0 {
				log.Warnf("Could not retrieve any peer device info for connection '%d'\n", conn.PeerNodeID)
				continue
			}
			for _, peerDev := range conn.PeerDevices {
				ch <- c.makeGaugeMetric("connections", float64(1), resource.Name, strconv.Itoa(conn.PeerNodeID),
					conn.PeerRole, strconv.Itoa(peerDev.Volume), strings.ToLower(peerDev.PeerDiskState))

				ch <- c.makeGaugeMetric("connections_sync", float64(peerDev.PercentInSync), resource.Name, strconv.Itoa(conn.PeerNodeID), strconv.Itoa(peerDev.Volume))

				ch <- c.makeGaugeMetric("connections_received", float64(peerDev.Received), resource.Name, strconv.Itoa(conn.PeerNodeID), strconv.Itoa(peerDev.Volume))

				ch <- c.makeGaugeMetric("connections_sent", float64(peerDev.Sent), resource.Name, strconv.Itoa(conn.PeerNodeID), strconv.Itoa(peerDev.Volume))

				ch <- c.makeGaugeMetric("connections_pending", float64(peerDev.Pending), resource.Name, strconv.Itoa(conn.PeerNodeID), strconv.Itoa(peerDev.Volume))

				ch <- c.makeGaugeMetric("connections_unacked", float64(peerDev.Unacked), resource.Name, strconv.Itoa(conn.PeerNodeID), strconv.Itoa(peerDev.Volume))

			}
		}
	}

}

func parseDrbdStatus(statusRaw []byte) ([]drbdStatus, error) {
	var drbdDevs []drbdStatus
	err := json.Unmarshal(statusRaw, &drbdDevs)
	if err != nil {
		return drbdDevs, err
	}
	return drbdDevs, nil
}

func (c *drbdCollector) recordDrbdSplitBrainMetric(ch chan<- prometheus.Metric) {

	// set split brain metric
	// by default if the custom hook is not set, the exporter will not be able to detect it
	files, _ := ioutil.ReadDir(c.drbdSplitBrainPath)

	// the split brain files exists
	for _, f := range files {
		// check if in directory there are file of syntax we expect (nil is when there is not any)
		match, _ := filepath.Glob(c.drbdSplitBrainPath + "/drbd-split-brain-detected-*")
		if match == nil {
			continue
		}
		resAndVolume := strings.Split(f.Name(), "drbd-split-brain-detected-")[1]

		// avoid to have index out range panic error (in case the there is not resource-volume syntax)
		if len(strings.Split(resAndVolume, "-")) != 2 {
			continue
		}
		//Resource (0) volume (1) place in slice
		resourceAndVolume := strings.Split(resAndVolume, "-")

		ch <- c.makeGaugeMetric("split_brain", float64(1), resourceAndVolume[0], resourceAndVolume[1])

	}
}
