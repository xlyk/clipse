# greet release checklist

## pre-release

- [ ] all tests pass (`make test`)
- [ ] linter clean (`make lint`)
- [ ] changelog entry written for this version
- [ ] version string bumped in source
- [ ] dependencies up to date; no known vulnerabilities

## build & verify

- [ ] binary builds without warnings (`make build`)
- [ ] `greet` binary runs locally and produces expected output
- [ ] smoke-tested on each target platform (linux/amd64, darwin/arm64)

## documentation

- [ ] README reflects any new flags or behaviour
- [ ] `--help` output is accurate
- [ ] any breaking changes called out explicitly

## release

- [ ] git tag created (`git tag vX.Y.Z`)
- [ ] tag pushed to origin
- [ ] GitHub release created with release notes
- [ ] release artefacts (binaries) attached to the GitHub release

## post-release

- [ ] confirm release appears on the GitHub releases page
- [ ] close or move associated Linear issues to done
- [ ] notify relevant stakeholders
