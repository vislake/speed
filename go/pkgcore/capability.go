package pkgcore

import (
	"strconv"
	"strings"
)

// Capability is a bitmask of properties a seam implementation declares about
// itself when it registers with a SeamRegistry, or when a host injects it
// through a KernelOption such as WithEventBus. Deployment mode and
// implementation composition are two orthogonal axes (see
// docs/internal/03-deployment-modes.md): the deployment mode never selects an
// implementation, it only states which capabilities the composition it runs
// must have, and Kernel.Bootstrap compares the two.
//
// The set is deliberately small today -- MultiReplicaSafe, SurvivesRestart
// and Stateless are the three properties docs/internal/03-deployment-modes.md's
// capability table names -- and is meant to grow by adding a row to that
// table and a bit here, never by adding a new kind of switch elsewhere.
type Capability uint8

const (
	// MultiReplicaSafe means the implementation may be shared by more than
	// one process at once: a write one replica makes is visible to another,
	// and no state the implementation holds is silently split per-process.
	// An in-process implementation -- a channel-backed EventBus, a map-backed
	// KVStore, a local-directory ObjectStore -- does not have it by
	// construction: each replica would get its own private copy. A
	// multi-replica deployment mode requires it of every seam that carries
	// shared state; a single-process deployment mode does not, because there
	// is only ever one replica to share state with.
	MultiReplicaSafe Capability = 1 << iota

	// SurvivesRestart means the implementation's state outlives the process
	// that wrote it: restarting the process does not silently drop data. An
	// in-memory KVStore, an in-process EventBus and a throwaway temporary
	// ObjectStore directory all lack it. Unlike MultiReplicaSafe, no
	// deployment mode requires this capability -- losing state across a
	// restart is a legitimate, deliberate choice for development or a
	// throwaway composition -- so Kernel.Bootstrap never fails an assembly
	// over its absence; it logs a startup warning instead, naming the seam
	// and the implementation, so the choice is visible rather than silent.
	SurvivesRestart

	// Stateless means the implementation keeps no state of its own in the
	// process: it hands each unit of work straight to whatever lies outside
	// it and keeps nothing back, so a restart drops nothing it holds.
	// mailer.console is the built-in example -- each Send writes the message
	// to its writer and returns, leaving no queue, buffer or connection
	// behind. The bit exists so assembly can tell an implementation with
	// nothing to lose from one that holds state but does not declare
	// SurvivesRestart: Kernel.Bootstrap warns about the latter at startup
	// and stays silent about the former, whose warning would name no loss at
	// all (see warnIfNotDurable).
	Stateless

	// KeyNeverLeavesBoundary means a private key this implementation
	// protects never exists in plaintext inside this process's memory --
	// docs/internal/22-pki.md's "capability declarations" section names it
	// in exactly those terms. Unlike the three bits above, this one is not
	// about an infrastructure seam's own state; it is declared by go/pki's
	// Signer implementations,
	// which self-register through a pkgcore.SeamRegistry[Signer] the
	// identical way EventBus/KVStore/Mailer/ObjectStore implementations do.
	// go/pki's LocalSigner does not have it: Sign decrypts the private key
	// into this process's memory for the duration of one signing call. A
	// KMS- or Vault-backed Signer has it only in direct-sign mode, where the
	// key is generated inside, and never leaves, the external service -- the
	// same implementation running in envelope mode (the real key encrypted
	// externally but decrypted locally to sign) does not have it either, for
	// the same reason LocalSigner does not. No deployment mode requires this
	// capability the way DeploymentModeDistributed requires MultiReplicaSafe
	// -- a high-security deployment declares it wants it when assembling
	// go/pki, and Kernel.Bootstrap's ordinary ErrCapabilityUnsatisfied check
	// is what enforces that, exactly as it does for the three bits above.
	KeyNeverLeavesBoundary
)

// capabilityNames lists every named bit in declaration order, so String
// renders a stable, readable form ("MultiReplicaSafe|SurvivesRestart")
// instead of a raw integer.
var capabilityNames = []struct {
	bit  Capability
	name string
}{
	{MultiReplicaSafe, "MultiReplicaSafe"},
	{SurvivesRestart, "SurvivesRestart"},
	{Stateless, "Stateless"},
	{KeyNeverLeavesBoundary, "KeyNeverLeavesBoundary"},
}

// Has reports whether c declares every bit set in want. An empty want (the
// zero Capability) is satisfied by any c, including the zero Capability
// itself -- the shape DeploymentModeStandalone's requirement takes, since a
// single-process deployment mode requires nothing extra of any seam.
func (c Capability) Has(want Capability) bool {
	return c&want == want
}

// String renders c as the pipe-joined names of its set bits, in the order
// they are declared above, or "none" for the zero Capability. An unnamed bit
// -- there are none yet, but the type leaves room to grow -- renders as its
// own hex literal rather than being silently dropped, so a future bit added
// here without a String update still shows up in an error message.
func (c Capability) String() string {
	if c == 0 {
		return "none"
	}

	var names []string
	remaining := c
	for _, known := range capabilityNames {
		if remaining&known.bit != 0 {
			names = append(names, known.name)
			remaining &^= known.bit
		}
	}
	if remaining != 0 {
		names = append(names, "Capability(0x"+strconv.FormatUint(uint64(remaining), 16)+")")
	}
	return strings.Join(names, "|")
}
