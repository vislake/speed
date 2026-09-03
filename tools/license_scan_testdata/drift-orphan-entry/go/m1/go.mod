module example.test/m1

go 1.25

require (
	a.example/alpha v1.0.0
	b.example/beta v1.2.0
)

require (
	z.example/zeta v0.0.1 // indirect
)
