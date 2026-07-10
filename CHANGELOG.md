# Changelog

## [1.1.0](https://github.com/bojanrajkovic/runny/compare/v1.0.2...v1.1.0) (2026-07-10)


### Features

* **app:** render benign cycle endings distinctly in the timeline ([#239](https://github.com/bojanrajkovic/runny/issues/239)) ([798243f](https://github.com/bojanrajkovic/runny/commit/798243f5a2018ead36bf287e3bcfed07633577a8))
* **ci:** report combined Go+Swift coverage to Codecov ([#281](https://github.com/bojanrajkovic/runny/issues/281)) ([427abbf](https://github.com/bojanrajkovic/runny/commit/427abbf12081181a8f8c848c0cc7789bf78e1eb7)), closes [#277](https://github.com/bojanrajkovic/runny/issues/277)
* **config:** add per-pool guest_env and guest_setup for guest provisioning ([#294](https://github.com/bojanrajkovic/runny/issues/294)) ([15a5b2e](https://github.com/bojanrajkovic/runny/commit/15a5b2e32db29497f0de5c0c1b1bee0cdcab1c89))
* **cycle:** persist the ending classification on the record ([#237](https://github.com/bojanrajkovic/runny/issues/237)) ([68f8adf](https://github.com/bojanrajkovic/runny/commit/68f8adfc15092eee86f6e6311ae35f950beac794))
* **guest:** add ssh_hardening: scramble to randomize guest passwords ([#215](https://github.com/bojanrajkovic/runny/issues/215)) ([1bc753d](https://github.com/bojanrajkovic/runny/commit/1bc753d51865495b55699304df580abdd312efea)), closes [#210](https://github.com/bojanrajkovic/runny/issues/210)
* **guest:** record operator debug sessions as cycle artifacts ([#211](https://github.com/bojanrajkovic/runny/issues/211)) ([c1c8bea](https://github.com/bojanrajkovic/runny/commit/c1c8bea409e2fa692fee87e4dede781fac4f044e))
* **home:** add opt-in observability.otlp config block ([#236](https://github.com/bojanrajkovic/runny/issues/236)) ([f77d859](https://github.com/bojanrajkovic/runny/commit/f77d859f1c721b01d3f82f7e08e8dda8a0ae8dd9))
* **images:** action events and ensurer-scope pull metrics ([#246](https://github.com/bojanrajkovic/runny/issues/246)) ([1d1f998](https://github.com/bojanrajkovic/runny/commit/1d1f99848bbbb9ea2afc0c70c3ab55166d340470))
* **obs,images:** pull-scoped events from the shared image puller ([#265](https://github.com/bojanrajkovic/runny/issues/265)) ([53d7f99](https://github.com/bojanrajkovic/runny/commit/53d7f99efc9403eec83c79880a2b47a67d61170f)), closes [#257](https://github.com/bojanrajkovic/runny/issues/257)
* **obs:** add the cycle observability event stream and Action wrapper ([#238](https://github.com/bojanrajkovic/runny/issues/238)) ([c7c6c7b](https://github.com/bojanrajkovic/runny/commit/c7c6c7b3841a009fe8d617a4822c3a1ad71e9dda))
* **obs:** emit spans for HTTP egress ([#248](https://github.com/bojanrajkovic/runny/issues/248)) ([9d18860](https://github.com/bojanrajkovic/runny/commit/9d188600c972f2d38d3b5da3f126ea0613f30e9d))
* **otlp:** send custom headers on OTLP exports, with ${env:VAR} expansion ([#273](https://github.com/bojanrajkovic/runny/issues/273)) ([283bee4](https://github.com/bojanrajkovic/runny/commit/283bee4d05cb18820b78bf06f0f1ca32fe19d61b))
* **release:** add a beta channel formula/cask pair to the tap ([#268](https://github.com/bojanrajkovic/runny/issues/268)) ([6cdb3c2](https://github.com/bojanrajkovic/runny/commit/6cdb3c25355f4f7353ae56d1eb3673dcc2fdf3a8))
* **runnyctl:** on-demand disk reclaim via runnyctl prune ([#206](https://github.com/bojanrajkovic/runny/issues/206)) ([5596ab9](https://github.com/bojanrajkovic/runny/commit/5596ab9913c5696083451e05240014ebaa79c3ae))
* **runnyctl:** stage config on install, add edit-config ([#274](https://github.com/bojanrajkovic/runny/issues/274)) ([6d467ca](https://github.com/bojanrajkovic/runny/commit/6d467ca595b7457a184ef0c7f8621eb8e038d7fa))
* **runnyd:** add the OTLP telemetry runtime — providers, resource, bounded shutdown ([#242](https://github.com/bojanrajkovic/runny/issues/242)) ([cd78eca](https://github.com/bojanrajkovic/runny/commit/cd78eca9350e9d1552f028a5b8c55bbc77935d37))
* **socket:** enforce operator revocation at RPC-start, not just connect() ([#270](https://github.com/bojanrajkovic/runny/issues/270)) ([e68ca57](https://github.com/bojanrajkovic/runny/commit/e68ca57384f13ce60717e8c6d60698e941ae6798))
* **socket:** grant/revoke/list operator accounts at runtime ([#220](https://github.com/bojanrajkovic/runny/issues/220)) ([8979276](https://github.com/bojanrajkovic/runny/commit/8979276ab05f3961c6063bbbbe16268993b0df66)), closes [#209](https://github.com/bojanrajkovic/runny/issues/209)
* **socket:** log the authenticated operator on lifecycle commands ([#272](https://github.com/bojanrajkovic/runny/issues/272)) ([74e3080](https://github.com/bojanrajkovic/runny/commit/74e3080adb7ac8a7a48c510a82d310b50bc073bd))
* **socket:** stamp the authenticated operator uid on injected_keys ([#219](https://github.com/bojanrajkovic/runny/issues/219)) ([c7a2786](https://github.com/bojanrajkovic/runny/commit/c7a2786f9fde34db14b4121994497d0a7a7cad8d))
* **statemachine:** action events for teardown, rotation, and provision sub-steps ([#244](https://github.com/bojanrajkovic/runny/issues/244)) ([127239a](https://github.com/bojanrajkovic/runny/commit/127239a89b3018759f2d030017326503d5f2de1f)), closes [#231](https://github.com/bojanrajkovic/runny/issues/231)
* **statemachine:** emit observability events from the record helpers ([#241](https://github.com/bojanrajkovic/runny/issues/241)) ([a3a5f37](https://github.com/bojanrajkovic/runny/commit/a3a5f3703db5cb5f41cea12e34d9acb6891f7f29)), closes [#224](https://github.com/bojanrajkovic/runny/issues/224)
* **telemetry:** add the trace emitter ([#243](https://github.com/bojanrajkovic/runny/issues/243)) ([fe9b82f](https://github.com/bojanrajkovic/runny/commit/fe9b82fc23768d66f88e8d040aeb955ae6a987c8)), closes [#227](https://github.com/bojanrajkovic/runny/issues/227)
* **telemetry:** fold pull events into traces and metrics; delete images.Metrics ([#266](https://github.com/bojanrajkovic/runny/issues/266)) ([1a89323](https://github.com/bojanrajkovic/runny/commit/1a8932344d059cf0515846e3e7482637ad6ed126)), closes [#258](https://github.com/bojanrajkovic/runny/issues/258)
* **telemetry:** metrics emitter — cycle instruments and slot gauges ([#245](https://github.com/bojanrajkovic/runny/issues/245)) ([d947877](https://github.com/bojanrajkovic/runny/commit/d947877e51d91df97510e5a6d9a5e886116a7ae8))


### Bug Fixes

* address post-1.0 security audit findings ([#201](https://github.com/bojanrajkovic/runny/issues/201)) ([ba32aae](https://github.com/bojanrajkovic/runny/commit/ba32aae9783baec5533458f2f4e9fdfcff1fbc57))
* collapse the runner-tarball cache to per-cycle owned clones ([#205](https://github.com/bojanrajkovic/runny/issues/205)) ([973b031](https://github.com/bojanrajkovic/runny/commit/973b031855ab7509294e29d7820b89eba9751689))
* **deps:** update module golang.org/x/sync to v0.22.0 ([#293](https://github.com/bojanrajkovic/runny/issues/293)) ([698859c](https://github.com/bojanrajkovic/runny/commit/698859cba9aaa1906436f318d2d36ae000c19898))
* **deps:** update module golang.org/x/sys to v0.47.0 ([#292](https://github.com/bojanrajkovic/runny/issues/292)) ([0a70309](https://github.com/bojanrajkovic/runny/commit/0a70309cee067e426a596be7cae925094fde77aa))
* **deps:** update module google.golang.org/grpc to v1.82.0 ([#289](https://github.com/bojanrajkovic/runny/issues/289)) ([e8d95bb](https://github.com/bojanrajkovic/runny/commit/e8d95bb14d721d18b2423eb811e231a06b413d44))
* harden daemon failure paths — force-stop, gRPC, stall, runner cache ([#203](https://github.com/bojanrajkovic/runny/issues/203)) ([a672c37](https://github.com/bojanrajkovic/runny/commit/a672c376830722d1bef4603b77d61aec8f0fee5f))
* **home:** validate retention.cycles_per_slot, share the positive-duration check ([#286](https://github.com/bojanrajkovic/runny/issues/286)) ([f118682](https://github.com/bojanrajkovic/runny/commit/f118682a1c60ab0ea88831954d6e834c9e59353b))
* **release:** pin version.sh's svu baseline to the last stable tag ([da62db0](https://github.com/bojanrajkovic/runny/commit/da62db09ecd0dfb0a621f02b24cf3bd2a9636517))
* **sshx:** stop WaitFor's rejection test racing real SSH timing ([#216](https://github.com/bojanrajkovic/runny/issues/216)) ([e391020](https://github.com/bojanrajkovic/runny/commit/e3910205903659cb013618fbdc4ae0a7ab2db167))
* **statemachine:** stop daemon shutdown from relabeling a decided cycle's ending ([#240](https://github.com/bojanrajkovic/runny/issues/240)) ([f46bd09](https://github.com/bojanrajkovic/runny/commit/f46bd09a67b438605b5e3a1fbe5e5a28eb0bf425))
* **sysdaemon:** parse the -test-config verdict at install time ([#282](https://github.com/bojanrajkovic/runny/issues/282)) ([0a35a2b](https://github.com/bojanrajkovic/runny/commit/0a35a2becda7c733252e5c867f83ea09d1b1485b))
* **vm:** reuse the persisted ECID and attach the tart-guest-agent's console port ([#222](https://github.com/bojanrajkovic/runny/issues/222)) ([efcc368](https://github.com/bojanrajkovic/runny/commit/efcc3681bb5ab3031be56fecedca24f49f155804))


### Refactoring

* apply the over-engineering audit across daemon, CLI, and app ([#276](https://github.com/bojanrajkovic/runny/issues/276)) ([80ad279](https://github.com/bojanrajkovic/runny/commit/80ad279d20801f62acc07c84c3d586ba692de58d))
* **guest:** extract a shared per-OS dispatch helper ([#217](https://github.com/bojanrajkovic/runny/issues/217)) ([3106113](https://github.com/bojanrajkovic/runny/commit/31061130854a4cc9b47830581dc9d92ae8b73342)), closes [#214](https://github.com/bojanrajkovic/runny/issues/214)
* **home:** fold limits.*/retention.max_age into a config field table ([#283](https://github.com/bojanrajkovic/runny/issues/283)) ([4af92ec](https://github.com/bojanrajkovic/runny/commit/4af92ec8ade8df202f63ffccd60f8854a134c634))
* **images:** collapse parameter lists into Ensurer methods ([#262](https://github.com/bojanrajkovic/runny/issues/262)) ([c38d48d](https://github.com/bojanrajkovic/runny/commit/c38d48d49d2dc1a3f6fe4dfa3a9d7284ccb90321)), closes [#253](https://github.com/bojanrajkovic/runny/issues/253)
* **obs,telemetry:** durations on step/job events; stateless metrics consumer ([#264](https://github.com/bojanrajkovic/runny/issues/264)) ([1254317](https://github.com/bojanrajkovic/runny/commit/12543176dacfdd5704eee0fb02870c2a63b19c05)), closes [#256](https://github.com/bojanrajkovic/runny/issues/256)
* **obs:** drop the unused Event.Seq / per-scope counter ([#267](https://github.com/bojanrajkovic/runny/issues/267)) ([28e3fa7](https://github.com/bojanrajkovic/runny/commit/28e3fa70b060f95ecc54e8839eeb034bde9f3548))
* **runnyctl:** parse the CLI with kong instead of stdlib flag ([#275](https://github.com/bojanrajkovic/runny/issues/275)) ([4e6523b](https://github.com/bojanrajkovic/runny/commit/4e6523b1151df9c5e8fd0f5d1b80ee9cce808f30))
* **statemachine:** extract the slot's mutex-guarded state into a statusCell ([#259](https://github.com/bojanrajkovic/runny/issues/259)) ([ceff3b2](https://github.com/bojanrajkovic/runny/commit/ceff3b207bfbcf3d96d8a0491d6ad4a1fc89eda8)), closes [#250](https://github.com/bojanrajkovic/runny/issues/250)
* **statemachine:** hoist runCycle's working state into a per-cycle run struct ([#260](https://github.com/bojanrajkovic/runny/issues/260)) ([6c3a599](https://github.com/bojanrajkovic/runny/commit/6c3a599a0b72a58f895ee7124bcdd231b4b0ca46)), closes [#251](https://github.com/bojanrajkovic/runny/issues/251)
* **statemachine:** unify the step bracket and the record/status/event writes ([#261](https://github.com/bojanrajkovic/runny/issues/261)) ([d957fca](https://github.com/bojanrajkovic/runny/commit/d957fcae2b1a2f48e87eee9528752187dcb302b3)), closes [#252](https://github.com/bojanrajkovic/runny/issues/252)
* **telemetry:** delete the deterministic trace-ID machinery ([#263](https://github.com/bojanrajkovic/runny/issues/263)) ([bfbf67e](https://github.com/bojanrajkovic/runny/commit/bfbf67e89aa0fb9e4487275f53021e41ec49a3cf)), closes [#255](https://github.com/bojanrajkovic/runny/issues/255)

## [1.0.2](https://github.com/bojanrajkovic/runny/compare/v1.0.1...v1.0.2) (2026-06-29)


### Bug Fixes

* **diskfree:** fall back to statfs(2) when CF API returns 0 ([#199](https://github.com/bojanrajkovic/runny/issues/199)) ([12a986a](https://github.com/bojanrajkovic/runny/commit/12a986a0cc8a49c250c101aec876f85e3e087fb6))

## [1.0.1](https://github.com/bojanrajkovic/runny/compare/v1.0.0...v1.0.1) (2026-06-29)


### Bug Fixes

* **doctor:** skip disk-headroom for already-cached images ([#197](https://github.com/bojanrajkovic/runny/issues/197)) ([f7eca3b](https://github.com/bojanrajkovic/runny/commit/f7eca3b5b97be6040640b9b7af5f65b5e48f99be))

## [1.0.0](https://github.com/bojanrajkovic/runny/compare/v0.3.0...v1.0.0) (2026-06-29)


### ⚠ BREAKING CHANGES

* graduate to stable 1.0

### Features

* graduate to stable 1.0 ([3938e56](https://github.com/bojanrajkovic/runny/commit/3938e566d8c3c256a34c7cf42f9efb6a170108ff))


### Bug Fixes

* **deploy:** preflight block blocks cask install when formula is present ([b93a086](https://github.com/bojanrajkovic/runny/commit/b93a0868d6d74d5fc576d1e37857e96bb9aec09c))


### Build

* drop --v0 from version.sh for 1.0 graduation ([825540d](https://github.com/bojanrajkovic/runny/commit/825540d92b1a1547cb6ea94d9a202447169874c8))
