package main

import (
	s "clichat/server"
	"context"
	"log"
	"os"
	"os/signal"
)

var server *s.ChatServer

func main() {
	log.SetFlags(0)

	err := run()
	if err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) < 2 {
		log.Println("please provide an address to listen on as the first argument")
		return nil
	}

	addr := os.Args[1]
	server = s.NewChatServer()
	log.Printf("starting server on http://%v", addr)
	go func() {
		err := server.ListenAndServe(addr)
		if err != nil {
			log.Printf("server exited with error: %v", err)
		}
	}()
	waitForShutdown()

	return nil
}

func waitForShutdown() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	<-sigs
	log.Println("shutting down")
	server.Shutdown(context.Background())
}
