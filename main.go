package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"
	"github.com/mridulganga/dlt-nodegroup/pkg/constants"
	mqttlib "github.com/mridulganga/dlt-nodegroup/pkg/mqttlib"
	"github.com/mridulganga/dlt-nodegroup/pkg/util"
	log "github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"
)

type Data map[string]any

var (
	nodegroupId    string
	nodegroupTopic string
	managerTopic   string
	mqttHost       string
	mqttPort       int

	isLoadtestActive bool
	loadTestId       string
	nodes            []string
	nodeUpdates      = map[string][]Data{}
	lastNodeUpdate   = map[string]Data{}
	nodeUpdatesLock  = sync.Mutex{}
)

func sendNodeGroupStatus(m mqttlib.MqttClient) {
	nodeUpdatesLock.Lock()
	defer nodeUpdatesLock.Unlock()

	payload := Data{
		"action":           "ng_update",
		"ng_status":        "healthy",
		"ng_id":            nodegroupId,
		"nodes":            nodes,
		"isLoadTestActive": isLoadtestActive,
		"timestamp":        fmt.Sprintf("%v", time.Now().Unix()),
	}

	if len(nodeUpdates) > 0 {
		resultBatchBytes, _ := json.Marshal(nodeUpdates)
		payload["node_updates"] = string(resultBatchBytes)
	}

	// check and update if load test is still active
	isLTActive := false
	for k := range lastNodeUpdate {
		if lastNodeUpdate[k]["isTestActive"].(bool) {
			isLTActive = true
			break
		}
	}
	isLoadtestActive = isLTActive

	if isLoadtestActive {
		payload["load_test_id"] = loadTestId
	}

	// reset node data
	for _, node := range nodes {
		if _, ok := nodeUpdates[node]; ok {
			nodeUpdates[node] = []Data{}
		}
	}

	m.Publish(managerTopic, payload)
}

func nodeHealthChecker() {
	for i, node := range nodes {
		if val, ok := lastNodeUpdate[node]["timestamp"]; ok {

			healthTime, _ := strconv.Atoi(val.(string))
			valTime := time.Unix(int64(healthTime), 0)

			if time.Since(valTime) > time.Second*20 {
				log.Infof("Removing node %s as no health update in 20 seconds", node)
				nodes = slices.Delete[[]string](nodes, i, i+1)
				delete(lastNodeUpdate, node)
				delete(nodeUpdates, node)
			}
		}
	}
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Error("Error loading .env file")
	}

	nodegroupId = os.Getenv(constants.NODEGROUP_ID)
	mqttHost = os.Getenv(constants.MQTT_HOST)
	mqttPort, _ = strconv.Atoi(os.Getenv(constants.MQTT_PORT))
	nodegroupTopic = fmt.Sprintf("ngs/%s", nodegroupId)
	managerTopic = "manager"

	healthCheckStopper := make(chan bool)

	/*
		start mqtt
		msg manager that ngx is active
			start periodic update messages to manager
			if node update batch available then collect and send to manager
		await messages on ng topic
			on: node update message
				collect messages into batches
			on: start_load_test
				distribute btwn nodes and send message to each
				to start load test
			on: stop_load_test
				tell all nodes to stop load test
	*/

	// start mqtt
	m := mqttlib.NewMqtt(mqttHost, mqttPort)
	go m.Connect()
	m.WaitUntilConnected()

	// publish message to nodegroup to add node (message contains node_id)
	m.Publish(managerTopic, Data{"action": "add_nodegroup", "ng_id": nodegroupId})

	// send periodic nodegroup status to manager
	go util.CallPeriodic(time.Second*5, func() {
		sendNodeGroupStatus(m)
	}, healthCheckStopper)

	go util.CallPeriodic(time.Second*5, func() {
		nodeHealthChecker()
	}, healthCheckStopper)

	// start listening to the messages sent to the node topic
	m.Sub(nodegroupTopic, func(client mqtt.Client, message mqtt.Message) {
		data := Data{}
		json.Unmarshal(message.Payload(), &data)
		// log.Infof("Received %v", data)

		switch data["action"] {
		case "node_update":
			// collect node updates
			nodeId := data["node_id"].(string)
			if slices.Contains(nodes, nodeId) {
				// append result if node is present
				nodeUpdatesLock.Lock()
				if nodeUpdates[nodeId] == nil {
					nodeUpdates[nodeId] = []Data{}
				}
				nodeUpdates[nodeId] = append(nodeUpdates[nodeId], data)
				lastNodeUpdate[nodeId] = data
				nodeUpdatesLock.Unlock()
			} else {
				// add node if not present
				nodes = append(nodes, nodeId)
			}
		case "start_loadtest":
			log.Infof("Starting Load Test, nodes %d", len(nodes))
			isLoadtestActive = true
			loadTestId = data["load_test_id"].(string)
			nodeCount := len(nodes)
			for _, node := range nodes {
				m.Publish(fmt.Sprintf("nodes/%s", node), Data{
					"action":       "start_loadtest",
					"load_test_id": data["load_test_id"],
					"plugin_data":  data["plugin_data"],
					"duration":     data["duration"],
					"tps":          int(data["tps"].(float64)) / nodeCount,
				})
			}
		case "stop_loadtest":
			log.Info("Stopping Load Test")
			for _, node := range nodes {
				m.Publish(fmt.Sprintf("nodes/%s", node), Data{
					"action": "stop_loadtest",
				})
			}
			isLoadtestActive = false
		}
	})

	// pause main thread
	select {}
}
