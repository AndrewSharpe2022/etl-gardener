// Package dispatch contains the logic for dispatching new reprocessing tasks.

package dispatch

import (
	"log"
	"math/rand"
	"time"

	"github.com/m-lab/etl-gardener/cloud/tq"
)

// DoDispatchLoop looks for next work to do.
// TODO - Just for proof of concept. Replace with more useful code.
func DoDispatchLoop(queuer *tq.QueueHandler) {
	for {
		log.Println("Dispatch Loop")
		// TODO - add content.

		stats, err := tq.GetTaskqueueStats(queuer.Project, queuer.Queue)
		if err != nil {
			log.Println(err)
		}
		log.Println(stats)

		time.Sleep(time.Duration(30+rand.Intn(60)) * time.Second)
	}
}