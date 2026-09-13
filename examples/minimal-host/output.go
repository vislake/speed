package main

import (
	"fmt"
	"os"
)

// say writes one line of this host's output.
//
// Every line the modules print goes through here so that the dropped write
// error is stated once rather than at each call site: stdout is the output
// channel itself, so a failed write to it has nowhere left to be reported and
// nothing to undo, and a run is not wrong because its narration did not land.
func say(line string) {
	//nolint:errcheck // dropping the error is what this function is for; the
	// doc comment above says why there is nothing to do with it.
	fmt.Fprintln(os.Stdout, line)
}
