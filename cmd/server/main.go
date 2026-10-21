package main

import (
	"log"
	"net/http"
	"os"

	"dsl/internal/temporalclient"
	"dsl/web"

	"go.temporal.io/sdk/client"
)

func main() {
	opts, profile, err := temporalclient.Options()
	if err != nil {
		log.Fatalln("unable to load Temporal client config:", err)
	}
	if profile == "" {
		profile = "none"
	}
	log.Printf("connecting to Temporal (profile %s)", profile)
	c, err := client.Dial(opts)
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
