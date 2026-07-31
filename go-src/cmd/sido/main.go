package main

import (
	"os"

	"sido-go/internal/sido"
)

func main() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "askpass":
		sido.AskpassMain(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "_inner_prompt_receiver":
		sido.ReceiverMain(os.Args[2:])
	default:
		sido.ManagerMain()
	}
}
