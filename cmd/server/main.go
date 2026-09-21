package main

import (
	"log"
	"net/http"
	"os"

	"dsl/web"

	"go.temporal.io/sdk/client"
)

func main() {
	c, err := client.Dial(client.Options{HostPort: hostPort()})
	if err != nil {
		log.Fatalln("unable to reach Temporal:", err)
	}
	defer c.Close()

	addr := ":8080"
	if v := os.Getenv("PORT"); v != "" {
		addr = ":" + v
	}

	log.Println("listening on http://localhost" + addr)
	if err := http.ListenAndServe(addr, web.NewServer(c).Routes()); err != nil {
		log.Fatalln("server stopped:", err)
	}
}

func hostPort() string {
	if v := os.Getenv("TEMPORAL_ADDRESS"); v != "" {
		return v
	}
	return client.DefaultHostPort
}
