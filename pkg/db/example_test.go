package db_test

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/db"
)

// ExampleClose shows the shape a call site uses with a connection it built
// itself: the close goes to a defer beside the Open it belongs to, so the
// connection is released on every path out of the call.
//
// That defer runs after a failed Open as well, and then the local the handle
// would have arrived in is nil. Reporting an error on nothing would ask every
// call site to test for a handle first, over an error with no remedy behind it:
// there is no connection left to release, which is the state closing is trying
// to reach.
func ExampleClose() {
	var handle *gorm.DB // what Open hands back when it failed

	fmt.Println(db.Close(handle))
	// Output: <nil>
}
