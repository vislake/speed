package main

// fragment_wire_paths_test.go holds the wire paths of the reference
// app's own spec-fragment surfaces (cases and smile-simulation) as
// test-local constants, since the P3a round removed the production
// constants the pre-fragment routes used to register themselves: the
// app's handlers now mount through the generated api.HandlerFromMux
// helpers, whose method+path patterns are derived from
// internal/cases/api/openapi.yaml and internal/smilesim/api/openapi.yaml
// themselves. These constants therefore pin what the spec fragments
// declare -- they are the flow tests' anchor on the wire, not a second
// copy of path truth production code keeps in step: a spec path change
// re-routes the live server (through the regenerated mount) and fails
// any flow test whose URL still names the old path, exactly as a
// contract change should surface.
const (
	// smileSimulatePath is the smile-simulation fragment's enqueue route
	// (operationId smilesim_simulate): POST it with a
	// smilesim.SimulateRequest body, and the 202 answer carries the async
	// job's id.
	smileSimulatePath = "/api/v1/smile-simulation/simulate"

	// smileJobPathPrefix is the fixed prefix of the smile-simulation
	// fragment's job-status route (operationId smilesim_getJob): GET
	// smileJobPathPrefix+"{jobID}" polls the job.
	smileJobPathPrefix = "/api/v1/smile-simulation/jobs/"

	// casesPath is the cases fragment's list-and-create route
	// (operationIds cases_listCases and cases_createCase).
	casesPath = "/api/v1/cases"

	// caseDetailPathPrefix is the fixed prefix of the cases fragment's
	// detail route (operationId cases_getCase): GET
	// caseDetailPathPrefix+"{caseId}" reads one case.
	caseDetailPathPrefix = casesPath + "/"
)
