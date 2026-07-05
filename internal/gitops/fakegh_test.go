package gitops

// fakeGhScript is a POSIX-shell stand-in for the real `gh` binary, written
// to a temp dir and PATH-shimmed in front of it by writeFakeGh/prependPath
// (see testhelpers_test.go). Every scenario-driven test in this package
// sets $FAKE_GH_SCENARIO before calling Run so ONE script serves every
// case -- mirroring testworker's own $TESTWORKER_SCENARIO convention
// (see AGENTS.md's "Testing philosophy") rather than generating a
// bespoke script per test.
//
// $FAKE_GH_STATE_DIR holds a marker file ("updated") the stale-base
// scenarios use to distinguish a `gh pr view` call before vs after this
// same test's `gh pr update-branch` call -- each invocation is a separate
// process, so a filesystem marker is the simplest way to thread state
// across them.
//
// $FAKE_GH_CALLLOG, when set, gets one line per invocation (the argv,
// space-joined) appended to it, so a test can assert exactly which gh
// commands Run issued without caring what this script returned.
const fakeGhScript = `#!/bin/sh
set -eu

if [ -n "${FAKE_GH_CALLLOG:-}" ]; then
	printf '%s\n' "$*" >> "$FAKE_GH_CALLLOG"
fi

scenario="${FAKE_GH_SCENARIO:-mergeable}"
state_dir="${FAKE_GH_STATE_DIR:-.}"
updated_marker="$state_dir/updated"

sub1="${1:-}"
sub2="${2:-}"

case "$sub1" in
pr)
	case "$sub2" in
	view)
		case "$scenario" in
		already_merged)
			echo '{"number":7,"url":"https://github.com/x/y/pull/7","state":"MERGED","mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN"}'
			;;
		draft)
			echo '{"number":7,"url":"https://github.com/x/y/pull/7","mergeable":"MERGEABLE","mergeStateStatus":"DRAFT","isDraft":true}'
			;;
		update_branch_refused)
			echo '{"number":7,"url":"https://github.com/x/y/pull/7","mergeable":"CONFLICTING","mergeStateStatus":"DIRTY"}'
			;;
		stale_base_merges | stale_base_conflict)
			if [ -f "$updated_marker" ]; then
				if [ "$scenario" = "stale_base_conflict" ]; then
					echo '{"number":7,"url":"https://github.com/x/y/pull/7","mergeable":"CONFLICTING","mergeStateStatus":"DIRTY"}'
				else
					echo '{"number":7,"url":"https://github.com/x/y/pull/7","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"}'
				fi
			else
				echo '{"number":7,"url":"https://github.com/x/y/pull/7","mergeable":"MERGEABLE","mergeStateStatus":"BEHIND"}'
			fi
			;;
		*)
			echo '{"number":7,"url":"https://github.com/x/y/pull/7","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"}'
			;;
		esac
		;;
	checks)
		case "$scenario" in
		failing_checks)
			echo '[{"name":"build","bucket":"fail"}]'
			exit 1
			;;
		absent_checks)
			echo '[]'
			;;
		ci_pending)
			echo '[{"name":"build","bucket":"pending"}]'
			exit 8
			;;
		*)
			echo '[{"name":"build","bucket":"pass"}]'
			;;
		esac
		;;
	merge)
		case "$scenario" in
		merge_fails)
			echo 'not all required checks have passed' >&2
			exit 1
			;;
		merge_not_ready)
			echo 'X Pull request x/y#7 is not mergeable: the base branch policy prohibits the merge.' >&2
			echo 'To have the pull request merged after all the requirements have been met, add the --auto flag.' >&2
			exit 1
			;;
		*)
			echo 'https://github.com/x/y/pull/7'
			;;
		esac
		;;
	update-branch)
		case "$scenario" in
		update_branch_refused)
			echo 'GraphQL: merge conflict between base and head' >&2
			exit 1
			;;
		*)
			mkdir -p "$state_dir"
			touch "$updated_marker"
			echo 'Updated branch clp-1 with latest changes from main'
			;;
		esac
		;;
	ready)
		echo 'https://github.com/x/y/pull/7'
		;;
	*)
		echo "fakegh: unknown pr subcommand: $sub2" >&2
		exit 2
		;;
	esac
	;;
api)
	case "$scenario" in
	protection_unsatisfied)
		echo 'HTTP 404: Branch not protected' >&2
		exit 1
		;;
	*)
		echo '{"required_status_checks":{"strict":true,"contexts":[]}}'
		;;
	esac
	;;
*)
	echo "fakegh: unknown command: $sub1" >&2
	exit 2
	;;
esac
`
