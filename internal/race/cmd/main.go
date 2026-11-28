// Command race prints how the same stream of jobs fares on fixed runners and
// on pumasi (package race), and with -out writes the film's run as JSON.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/midagedev/pumasi/internal/race"
)

func main() {
	out := flag.String("out", "", "write the film scenario's runs, frame by frame, to this JSON file")
	flag.Parse()
	r := race.Run(race.Film)
	fmt.Print(r.Table())
	fmt.Println()
	fmt.Print(race.Sweep())
	fmt.Println()
	fmt.Print(race.ETATable())
	if *out != "" {
		b, err := json.Marshal(r)
		if err == nil {
			err = os.WriteFile(*out, b, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
