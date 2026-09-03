package upgrade

import (
	"fmt"
)

// ExampleRewrite shows the pure rewrite contract: the single speed module
// require moves to the target version while the third-party require block
// and its comments survive byte for byte.
func ExampleRewrite() {
	mod := []byte(`module example.com/app

go 1.25.0

// Speed modules move in lockstep; the third-party block does not.
require github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
)
`)
	out, changed, err := Rewrite(mod, "v1.0.0")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("changed: %d\n", changed)
	fmt.Print(string(out))
	// Output: changed: 1
	// module example.com/app
	//
	// go 1.25.0
	//
	// // Speed modules move in lockstep; the third-party block does not.
	// require github.com/vislake/speed/go/authn v1.0.0
	//
	// require (
	// 	github.com/BurntSushi/toml v1.6.0 // indirect
	// )
}
