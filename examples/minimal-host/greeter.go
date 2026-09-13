package main

// Greeter is the capability this example is built around. Two modules deliver
// it and the application module takes it up, without either side naming the
// other: the interface is the whole of the contract between them.
type Greeter interface {
	// Greet renders the greeting for a name.
	Greet(name string) string
	// Endpoint names where the greeting came from, which is what makes the
	// chosen implementation visible from the outside.
	Endpoint() string
}
