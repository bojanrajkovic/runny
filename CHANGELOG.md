# Changelog

## [1.2.0](https://github.com/bojanrajkovic/runny/compare/v1.1.0...v1.2.0) (2026-09-02)


### Features

* cross-compile the daemon and CLI for windows/amd64 ([#301](https://github.com/bojanrajkovic/runny/issues/301)) ([79c236b](https://github.com/bojanrajkovic/runny/commit/79c236b8064fb289bf86bbb1a51f6b18ed58f956))
* **diskfree:** add Windows implementation ([#299](https://github.com/bojanrajkovic/runny/issues/299)) ([10d0504](https://github.com/bojanrajkovic/runny/commit/10d0504ed0e619217f04d955600e029b46d13bf6))
* **guest:** pull runner diag logs whole instead of a 32KiB tail ([#377](https://github.com/bojanrajkovic/runny/issues/377)) ([4b225b5](https://github.com/bojanrajkovic/runny/commit/4b225b56afc6b3b2de938d9351deb9b00d5c6481))
* **guest:** record windows debug sessions via a forced-command recorder ([#355](https://github.com/bojanrajkovic/runny/issues/355)) ([42fdb7f](https://github.com/bojanrajkovic/runny/commit/42fdb7f012366fe4ee48161f11b78d245a8f5888))
* **obs:** publish the guest's resolved shape on vm_info ([#367](https://github.com/bojanrajkovic/runny/issues/367)) ([6c9267d](https://github.com/bojanrajkovic/runny/commit/6c9267d50aec2ccc9edb6877ed995ee2bff6efc6))
* **oci:** add runnyctl image pack, a tart-format OCI Image Layout writer ([#340](https://github.com/bojanrajkovic/runny/issues/340)) ([493f9bd](https://github.com/bojanrajkovic/runny/commit/493f9bdf5cba1eede8b1a95b801db19edd332ce5))
* **oci:** support credentialed registry pulls ([#349](https://github.com/bojanrajkovic/runny/issues/349)) ([38610e5](https://github.com/bojanrajkovic/runny/commit/38610e5d5e1d017ae859c6a233a425f5a32820ee)), closes [#350](https://github.com/bojanrajkovic/runny/issues/350)
* **opacl,socket:** windows live operator grant/revoke with platform-native SID identity ([#330](https://github.com/bojanrajkovic/runny/issues/330)) ([bf553b2](https://github.com/bojanrajkovic/runny/commit/bf553b2584d5e306f89eeb03c0d10dd2597ed86b))
* **release:** publish windows runnyctl/runnyd release artifacts ([#356](https://github.com/bojanrajkovic/runny/issues/356)) ([8380a10](https://github.com/bojanrajkovic/runny/commit/8380a10f1fe160f63c30a826ae70a5d71daffc3c)), closes [#342](https://github.com/bojanrajkovic/runny/issues/342)
* **release:** ship a self-hosted Chocolatey feed, drop winget ([#358](https://github.com/bojanrajkovic/runny/issues/358)) ([0ee5201](https://github.com/bojanrajkovic/runny/commit/0ee520122c07cc3dfaa0d2c50854ae9897751d42))
* run Windows guests through the full runner cycle ([#346](https://github.com/bojanrajkovic/runny/issues/346)) ([a521a18](https://github.com/bojanrajkovic/runny/commit/a521a189f6c7fd0be73cb808f580d661538c6e69))
* **runnyctl:** verify the control-pipe server's owner before trusting it ([#336](https://github.com/bojanrajkovic/runny/issues/336)) ([2c10e38](https://github.com/bojanrajkovic/runny/commit/2c10e38479cd784ff792d287efa0a6956fbff637))
* **runnyd:** run under the Windows Service Control Manager ([#310](https://github.com/bojanrajkovic/runny/issues/310)) ([db45f04](https://github.com/bojanrajkovic/runny/commit/db45f04a38709b3d860ef6fe1956032704f30d76)), closes [#303](https://github.com/bojanrajkovic/runny/issues/303)
* **socket:** arm the windows operator-revocation gate over a named pipe ([#333](https://github.com/bojanrajkovic/runny/issues/333)) ([f5497c1](https://github.com/bojanrajkovic/runny/commit/f5497c136e1d695d15c4c305363c06d210fbf53c))
* **sysdaemon:** add the Windows SCM installer ([#311](https://github.com/bojanrajkovic/runny/issues/311)) ([35989e3](https://github.com/bojanrajkovic/runny/commit/35989e3d3c3892926b7918d5d09597812d6755cf)), closes [#302](https://github.com/bojanrajkovic/runny/issues/302)
* **vhdx:** add in-process raw-to-fixed-VHDX converter ([#313](https://github.com/bojanrajkovic/runny/issues/313)) ([01accd4](https://github.com/bojanrajkovic/runny/commit/01accd4c0ee3fcb2c617c687062014fa4dbeb0a3))
* **vhdx:** differencing-disk clone, parent-locator reader, prune parent-reference check ([#315](https://github.com/bojanrajkovic/runny/issues/315)) ([7e8e977](https://github.com/bojanrajkovic/runny/commit/7e8e9779f10fb76f6c82006ebef7481f9cfc710c))
* **vm:** add Hyper-V VM backend for windows (HCS compute systems) ([#318](https://github.com/bojanrajkovic/runny/issues/318)) ([54189d1](https://github.com/bojanrajkovic/runny/commit/54189d17a2abbe19e47a7e49e8412c43a6f725e4)), closes [#308](https://github.com/bojanrajkovic/runny/issues/308)
* **vm:** add RunnerShareDir support for the Hyper-V backend ([#324](https://github.com/bojanrajkovic/runny/issues/324)) ([8325c26](https://github.com/bojanrajkovic/runny/commit/8325c268f697421cc685ba65b216f238e17402e1)), closes [#319](https://github.com/bojanrajkovic/runny/issues/319) [#323](https://github.com/bojanrajkovic/runny/issues/323)
* **vm:** support Windows guests in the Hyper-V/HCS boot path ([#339](https://github.com/bojanrajkovic/runny/issues/339)) ([7581c63](https://github.com/bojanrajkovic/runny/commit/7581c632c68ffe91e0ae78aed894c9835827ed23))
* **windows:** make daemon upgrades work end to end ([#361](https://github.com/bojanrajkovic/runny/issues/361)) ([e050062](https://github.com/bojanrajkovic/runny/commit/e0500626fa1285ec25aa79b301d8c73c4cb49041))
* **winhcs:** vendor the HCS binding (trimmed, slog, OTel-bridge-ready) ([#314](https://github.com/bojanrajkovic/runny/issues/314)) ([56a78a2](https://github.com/bojanrajkovic/runny/commit/56a78a247e1b5f51718a3e3162d91112d5091e19))


### Bug Fixes

* **acl:** make a revoke reach every artifact, the way each platform allows ([#380](https://github.com/bojanrajkovic/runny/issues/380)) ([dada56c](https://github.com/bojanrajkovic/runny/commit/dada56c562930f85e728be223c72339a2af60b7e))
* **deps:** update module github.com/alecthomas/kong to v1.16.0 ([#321](https://github.com/bojanrajkovic/runny/issues/321)) ([dbc8fd7](https://github.com/bojanrajkovic/runny/commit/dbc8fd72a0f5128a8ac6839ebb0145248c29848c))
* **deps:** update module github.com/alecthomas/kong to v1.16.1 ([#402](https://github.com/bojanrajkovic/runny/issues/402)) ([abbb5d5](https://github.com/bojanrajkovic/runny/commit/abbb5d5032ea9e1bdfedbe9b8695b4448dd61eec))
* **deps:** update module github.com/pierrec/lz4/v4 to v4.1.28 ([#400](https://github.com/bojanrajkovic/runny/issues/400)) ([ff69944](https://github.com/bojanrajkovic/runny/commit/ff69944189f4e9140e2ce2e65fb83501793a2d16))
* **deps:** update module github.com/pierrec/lz4/v4 to v4.1.29 ([#409](https://github.com/bojanrajkovic/runny/issues/409)) ([d4494ec](https://github.com/bojanrajkovic/runny/commit/d4494ec2b8f2410899f12dee0ba5d3b4f29a3f70))
* **deps:** update module golang.org/x/crypto to v0.54.0 ([#297](https://github.com/bojanrajkovic/runny/issues/297)) ([2365ef7](https://github.com/bojanrajkovic/runny/commit/2365ef76d9e53482fbc3ff736d54874344c116fa))
* **deps:** update module golang.org/x/term to v0.45.0 ([#296](https://github.com/bojanrajkovic/runny/issues/296)) ([7054c01](https://github.com/bojanrajkovic/runny/commit/7054c016e29d4a9ed08ca467b4e7d00ba299682b))
* **deps:** update module google.golang.org/grpc to v1.82.1 [security] ([#317](https://github.com/bojanrajkovic/runny/issues/317)) ([43e485a](https://github.com/bojanrajkovic/runny/commit/43e485a3af5ebdf940c02fee12ce85dc669dc0b1))
* **deps:** update module google.golang.org/grpc to v1.83.0 ([#395](https://github.com/bojanrajkovic/runny/issues/395)) ([c3ad755](https://github.com/bojanrajkovic/runny/commit/c3ad75586baac00c55a7136fac046020387b290e))
* **deps:** update module google.golang.org/grpc to v1.83.1 ([#410](https://github.com/bojanrajkovic/runny/issues/410)) ([e4445dd](https://github.com/bojanrajkovic/runny/commit/e4445dda6489512c14079c0427d0f695ef641641))
* **deps:** update module google.golang.org/grpc to v1.83.2 ([#414](https://github.com/bojanrajkovic/runny/issues/414)) ([678f3c5](https://github.com/bojanrajkovic/runny/commit/678f3c5c6206a89530376bc472c6db03c9c9aff0))
* **deps:** update module google.golang.org/protobuf to v1.36.12 ([#403](https://github.com/bojanrajkovic/runny/issues/403)) ([783f4f4](https://github.com/bojanrajkovic/runny/commit/783f4f410221bd3d2ba7936988d2395557e76a5f))
* **deps:** update opentelemetry-go monorepo to v1.45.0 ([#399](https://github.com/bojanrajkovic/runny/issues/399)) ([b4e0c53](https://github.com/bojanrajkovic/runny/commit/b4e0c53c8a70e44f454f8aae286a2b7c9d47ed2e))
* **deps:** update opentelemetry-go monorepo to v1.46.0 ([#415](https://github.com/bojanrajkovic/runny/issues/415)) ([7b63333](https://github.com/bojanrajkovic/runny/commit/7b63333b0fee66c0b81609b57e3c9439256a4636))
* **guest:** disable keyboard-interactive auth on windows rotate, not just password ([#384](https://github.com/bojanrajkovic/runny/issues/384)) ([0ca8c39](https://github.com/bojanrajkovic/runny/commit/0ca8c391be8201765e8fc779f375dfee2447e44b))
* **guest:** read windows diag logs the runner still has open ([#375](https://github.com/bojanrajkovic/runny/issues/375)) ([e12fa34](https://github.com/bojanrajkovic/runny/commit/e12fa34219da7b51988d08d2af13d3c08dc2b9ae))
* **guest:** stop generating windows scramble passwords rand.Text can't pass ([#357](https://github.com/bojanrajkovic/runny/issues/357)) ([92ec165](https://github.com/bojanrajkovic/runny/commit/92ec16500ac1d0a76eca4d7f0d5149a7dab22651))
* **oci:** scope cached registry tokens to the repository they were minted for ([#368](https://github.com/bojanrajkovic/runny/issues/368)) ([0cc3a74](https://github.com/bojanrajkovic/runny/commit/0cc3a747bb4511054786a26237e83830292ff181))
* **release:** keep the nuspec template well-formed, and test that it is ([#364](https://github.com/bojanrajkovic/runny/issues/364)) ([69d743d](https://github.com/bojanrajkovic/runny/commit/69d743dd548abfde3cf6543dc2877be5bf3e65bb))
* **release:** make pre-release versions sort ([#366](https://github.com/bojanrajkovic/runny/issues/366)) ([42ff2e6](https://github.com/bojanrajkovic/runny/commit/42ff2e66a101cd61a72c68371ca3cfc5faad2c77))
* **runnyctl:** fall back to notepad.exe for edit-config on Windows ([#386](https://github.com/bojanrajkovic/runny/issues/386)) ([3c375c4](https://github.com/bojanrajkovic/runny/commit/3c375c41dd60d6825177fb19ced37979cbdd325b))
* **runnyd:** make -doctor's read-only contract true, not just documented ([#382](https://github.com/bojanrajkovic/runny/issues/382)) ([0c1aa04](https://github.com/bojanrajkovic/runny/commit/0c1aa04f215f9c74cf7ab10efab888c886e534c2))
* **runnyd:** resolve -doctor's home from an explicit -config, not the invoker's ([6be40e2](https://github.com/bojanrajkovic/runny/commit/6be40e285593a2cc2c2a47feaa465b3ab0895518)), closes [#351](https://github.com/bojanrajkovic/runny/issues/351)
* **socket:** stop any authenticated user adding an instance to the control pipe ([#370](https://github.com/bojanrajkovic/runny/issues/370)) ([92b0513](https://github.com/bojanrajkovic/runny/commit/92b051367c5b273629e8e1a4c9209fb389a4d055))
* **telemetry:** adapt vendored HCS spans into obs instead of bridging them ([#390](https://github.com/bojanrajkovic/runny/issues/390)) ([9f0916d](https://github.com/bojanrajkovic/runny/commit/9f0916d4cc28df624a860dac559b80f080545e9a))
* **vm,guest,sysdaemon:** close 4 review findings from PR [#324](https://github.com/bojanrajkovic/runny/issues/324) ([#325](https://github.com/bojanrajkovic/runny/issues/325)) ([258c0ed](https://github.com/bojanrajkovic/runny/commit/258c0edbda599e9e4c5a321c213f876fe622dcb7))
* **vm:** authenticate the guest console pipe and make its name unguessable ([#359](https://github.com/bojanrajkovic/runny/issues/359)) ([c4b515f](https://github.com/bojanrajkovic/runny/commit/c4b515f234384e59ce095d000c9c1e84961c057e))
* **vm:** bound the HNS calls that had no deadline at all ([#378](https://github.com/bojanrajkovic/runny/issues/378)) ([728e712](https://github.com/bojanrajkovic/runny/commit/728e71291dae0e979e182c03f1a58f28c6e09cfd))
* **vm:** keep the observability scope when force-stopping a guest ([#393](https://github.com/bojanrajkovic/runny/issues/393)) ([661c922](https://github.com/bojanrajkovic/runny/commit/661c922b474bd1cdc8054026a2be67b893e8cc22))
* **vm:** reap orphaned compute systems, identify VM backend in telemetry ([#326](https://github.com/bojanrajkovic/runny/issues/326)) ([85af183](https://github.com/bojanrajkovic/runny/commit/85af183759c4bb9150510937e4de6b9dda67c6ec))
* **vm:** return the guest's real lease IP from WaitIP, not the HNS pre-commit ([#334](https://github.com/bojanrajkovic/runny/issues/334)) ([e7b5a01](https://github.com/bojanrajkovic/runny/commit/e7b5a017c6c9200453fd4c2e18dff5278a22f977))
* **vm:** scope a slot's HCS system ID to the install, not just the slot name ([#372](https://github.com/bojanrajkovic/runny/issues/372)) ([c5a0454](https://github.com/bojanrajkovic/runny/commit/c5a04544d2c17818e7b344ff20f039cc7b7fb061))
* **vm:** stop attempting to detach the guest console ([#365](https://github.com/bojanrajkovic/runny/issues/365)) ([70910fa](https://github.com/bojanrajkovic/runny/commit/70910fa7538721b702292bbd35aff38119dff47b))
* **vm:** stop leaving a guest console pipe bound for the guest's lifetime ([#360](https://github.com/bojanrajkovic/runny/issues/360)) ([7c9b43e](https://github.com/bojanrajkovic/runny/commit/7c9b43ed53a98f4f0df9ca525b1b8d269c3ac748))
* **windows:** arm the guards that were shaped for unix and never fired ([#379](https://github.com/bojanrajkovic/runny/issues/379)) ([2ab9e1f](https://github.com/bojanrajkovic/runny/commit/2ab9e1f29363b408954517d1082d7e052693cb67))
* **winhcs:** flatten the double internal/ vendor nesting ([#316](https://github.com/bojanrajkovic/runny/issues/316)) ([996b1ca](https://github.com/bojanrajkovic/runny/commit/996b1caff03743c9e68ef9957e04a3e8f09f9e80))


### Refactoring

* **guest:** collapse exit-check boilerplate into a runStep helper ([#352](https://github.com/bojanrajkovic/runny/issues/352)) ([c50a57b](https://github.com/bojanrajkovic/runny/commit/c50a57b04ad0b12435e6d52304d310e03891c45d)), closes [#347](https://github.com/bojanrajkovic/runny/issues/347)
* **guest:** drop StartRunner's redundant goos parameter ([021ac30](https://github.com/bojanrajkovic/runny/commit/021ac30d88db0c541bbfaa27c9bf6d26228704fd)), closes [#345](https://github.com/bojanrajkovic/runny/issues/345)
* **winhcs:** drop the vendored packages two uncalled entry points kept alive ([#387](https://github.com/bojanrajkovic/runny/issues/387)) ([34fe4a7](https://github.com/bojanrajkovic/runny/commit/34fe4a75a0275bf706b4dd5a9a73f2c6d7cbf53a))

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
