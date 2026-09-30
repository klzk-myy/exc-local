// exchange-errblast is the Phase-08.5 Task 8.5.3.3 high-stress error
// injector (tests/load/errblast/). It lives OUTSIDE services/ so harness
// dependencies never pollute the service module — and it needs none: the
// tool speaks plain HTTP to the gateway (same convention as tests/soak,
// minus the internal/ipc import that forced the `replace` there).
module exchange/tests/load/errblast

go 1.26.8
