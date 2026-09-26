"""Build an asciicast v2 recording from real captured pikopod output.

Every byte of program output below came from running the real binary; only the
keystroke and pause timing is synthesized, which is what a recording is.
"""
import json
import time

ESC = chr(27)
GREEN = ESC + "[38;5;114m"
RESET = ESC + "[0m"

W, H = 120, 42
events = []
t = 0.0


def out(s):
    events.append([round(t, 3), "o", s])


def pause(d):
    global t
    t += d


def prompt():
    out(GREEN + "$" + RESET + " ")


def typed(cmd, cps=0.028):
    for ch in cmd:
        out(ch)
        pause(cps)
    pause(0.35)
    out("\r\n")


def emit_file(path, per_line=0.035):
    for line in open(path).read().splitlines():
        out(line + "\r\n")
        pause(per_line)


pause(0.6)
prompt()
typed("pikopod import examplepay --spec ./examplepay.spec.json")
emit_file("out_import.txt")
pause(1.6)

prompt()
typed("pikopod scenario list examplepay")
emit_file("out_list.txt")
pause(2.4)

prompt()
typed("pikopod scenario run examplepay declines retry_storm")
emit_file("out_run.txt", 0.05)
pause(2.6)

prompt()
typed("pikopod incidents")
emit_file("out_incidents.txt")
pause(2.0)

prompt()
typed("pikopod scenario reproduce fp_14835fa32dfb")
emit_file("out_reproduce.txt", 0.06)
pause(3.0)
prompt()
pause(1.2)

header = {
    "version": 2,
    "width": W,
    "height": H,
    "timestamp": int(time.time()),
    "env": {"SHELL": "/bin/bash", "TERM": "xterm-256color"},
    "title": "pikopod - rehearse the failures, reproduce the ones you missed",
}
with open("demo.cast", "w") as f:
    f.write(json.dumps(header) + "\n")
    for e in events:
        f.write(json.dumps(e) + "\n")

print("  events: %d   duration: %.1fs" % (len(events), t))
