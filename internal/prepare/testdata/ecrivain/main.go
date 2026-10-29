package main

import (
	"fmt"
	"os"
	"time"
)

// Image de test (docker_real_test.go) : écrit sans cesse un compteur dans
// /data/compteur.txt, pour vérifier que le gel fige le volume.
func main() {
	for i := 0; ; i++ {
		os.WriteFile("/data/compteur.txt", []byte(fmt.Sprint(i)), 0o644)
		time.Sleep(10 * time.Millisecond)
	}
}
