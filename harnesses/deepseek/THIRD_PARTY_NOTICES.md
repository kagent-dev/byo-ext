# Source provenance

This harness was developed in kagent PR #3195, source commit
`20ed48c1dfa711b08004c2346eb131bbaa36acb6`:
https://github.com/kagent-dev/kagent/pull/3195

`internal/runtimeutil` contains the private-directory, atomic-file replacement,
and process-group setup helpers derived from `go/harness/internal/utils` in
kagent at that commit, licensed under Apache-2.0. The functions are maintained
locally because Go forbids an external module from importing that internal
package. No core runtime or controller implementation is copied here.

The kagent Go dependency is pinned in go.mod to the merge of PR #3165:
`0a57368aca336ea5523c6e7ac3d551aa7e9f37f1`. Its dependency replacement for
Substrate is repeated explicitly because Go does not inherit dependency replace
directives. The upstream runtime owns public task restoration.

`.golangci.yaml` is adapted from the same kagent source under Apache-2.0.

DSH and its npm dependencies are fetched with package-lock.json and retain their
individual package licenses. DSH source is not modified.
