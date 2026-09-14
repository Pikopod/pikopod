# The README recording

`demo.gif` is the terminal recording at the top of the README. This directory
holds everything needed to regenerate it, so it is a reproducible artifact
rather than a binary nobody can check.

| File | What |
| --- | --- |
| `demo.gif` | The recording the README embeds (~113 KB) |
| `demo.cast` | The [asciicast v2](https://docs.asciinema.org/manual/asciicast/v2/) source — plain text, diffs in review |
| `mkcast.py` | Builds `demo.cast` from captured output |
| `examplepay.spec.json` | The spec the recording runs against |
| `out_list.txt`, `out_run.txt` | Captured output, regenerated each time |

## What is real in it

**Every byte of program output came from running the real binary** against
`examplepay.spec.json`. Only the keystroke timing and the pauses are
synthesized — which is what a recording is.

Nothing in it is hand-written prose pretending to be output. That matters here
more than in most projects: this repository has already shipped a README command
that 404'd and a verification command that could not pass, and a hand-edited
recording is the same failure wearing a costume.

## Regenerating

Needs [`agg`](https://docs.asciinema.org/manual/agg/) (`brew install agg`) and
Python 3.

```bash
cd "$(mktemp -d)"
cp <repo>/docs/demo/{examplepay.spec.json,mkcast.py} .
go build -o pikopod <repo>/cmd/pikopod
export PATH="$PWD:$PATH"

pikopod init
pikopod import examplepay --spec ./examplepay.spec.json
pikopod scenario list examplepay             > out_list.txt
pikopod scenario run examplepay declines retry_storm > out_run.txt

python3 mkcast.py
agg --theme asciinema --font-size 15 demo.cast demo.gif
```

Copy `demo.cast`, `demo.gif`, `out_list.txt` and `out_run.txt` back over the
ones here.

Run it in a scratch directory, not the repo: `pikopod init` writes a
`pikopod.yaml` and a `pikopod-data/` where you stand.

## Two things to know before you change it

**`demo.cast` is not byte-reproducible.** Its header carries a timestamp, so the
file changes on every regeneration even when nothing else does. Diff from line 2
if you want to see whether the *content* moved.

**Regenerate only when the output actually changes.** Every regeneration is a
new ~113 KB blob in git history, permanently. One committed GIF is nothing; one
per release is 30 MB nobody can prune. If `scenario list` grows a column, that
is worth a new recording. A tweak to the pause timings is not.

## Why not VHS

[VHS](https://github.com/charmbracelet/vhs) is the obvious tool and produces a
nicer tape format. It renders through a headless Chromium via go-rod, which
needs a browser download that does not always work in sandboxed or offline
environments — and when it fails it exits `0` and silently writes nothing.

The asciicast route has no browser in it, and its source artifact is text rather
than a binary, so a reviewer can see what changed. If you would rather use VHS,
that is a fine trade — keep the text output too.
