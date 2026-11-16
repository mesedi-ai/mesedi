package store

// AllFailureClasses is the one list of every failure class a detector can
// emit, in the order of their constants in store.go.
//
// Until 2026-11-16 each consumer kept its own copy: the webhook filter
// missed environment_misapprehension and covert_coordination (outside
// the outside review of 2026-11-15), and the severity defaults, the detector coverage
// gate and the dashboard lagged too (E16). Consumers now build from this
// list or are tested against it, and TestAllFailureClassesListsEveryConstant
// fails when a FailureClass constant is added without being listed here.
var AllFailureClasses = []string{
	FailureClassCrashes,
	FailureClassLoops,
	FailureClassToolFailures,
	FailureClassValidator,
	FailureClassDrift,
	FailureClassCostVelocity,
	FailureClassInjection,
	FailureClassInfraThrottled,
	FailureClassDataLeakage,
	FailureClassSemanticLoop,
	FailureClassToolSchemaDrift,
	FailureClassContextOverflow,
	FailureClassTokenWaste,
	FailureClassSandboxEscape,
	FailureClassEnvironmentMisapprehension,
	FailureClassCovertCoordination,
	FailureClassGroundingFailure,
	FailureClassCascadingFailure,
	FailureClassCoordinationDeadlock,
	FailureClassProviderIncident,
	FailureClassHITLTimeout,
	FailureClassHITLRejectionSpike,
	FailureClassRecordIntegrity,
}
