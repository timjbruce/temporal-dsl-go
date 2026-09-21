package main

import (
	"log"
	"os"
	"strconv"

	bank "dsl/workflow"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	c, err := client.Dial(client.Options{HostPort: hostPort()})
	if err != nil {
		log.Fatalln("unable to reach Temporal:", err)
	}
	defer c.Close()

	speed := speedFactor()
	w := worker.New(c, bank.TaskQueueName, worker.Options{})
	w.RegisterWorkflow(bank.PositionWorkflow)
	w.RegisterActivity(&bank.Activities{SpeedFactor: speed})

	log.Printf("worker listening on task queue %s (simulation speed %.3gx)", bank.TaskQueueName, speed)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalln("worker stopped:", err)
	}
}

// SIM_SPEED scales the simulated system latencies: below 1 they shrink, so a
// demo need not sit through loan servicing's full 35 seconds. Above 1 they
// stretch, and past about 1.8 a step outlasts its own StartToCloseTimeout and
// fails after its retries.
func speedFactor() float64 {
	v, err := strconv.ParseFloat(os.Getenv("SIM_SPEED"), 64)
	if err != nil || v <= 0 {
		return 1
	}
	return v
}

func hostPort() string {
	if v := os.Getenv("TEMPORAL_ADDRESS"); v != "" {
		return v
	}
	return client.DefaultHostPort
}
