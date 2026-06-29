# Changelog

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
