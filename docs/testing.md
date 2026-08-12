# Testing guidance

## The standing rule

**Anything that models an external binary's behaviour is verified against that
binary at least once, and the measurement is recorded here.**

Two defects shipped past a full test suite because a fake encoded an assumption
nobody had checked. Both were invisible to every test and obvious within
seconds of running the real thing.

- **Concurrent lanes shared one working tree.** A repo's `max_concurrent` above
  1 put several agents in the same checkout, sharing one index and one branch.
  Every test passed, because a fake executor has no index to corrupt. It
  appeared immediately against a real bare repo and a real clone.
- **The stall watchdog killed healthy lanes.** Every fake executor wrote output
  the moment it was called, so silence never occurred in a test. The real
  `claude -p` is silent for most of its run. Measurement below.

The pattern is the same both times: a mock is a statement about how something
else behaves, and an unverified statement is a guess. Write the fake, then go
and check the claim it rests on.

## Measurement: `claude -p` does not stream in its default mode

`claude -p` defaults to `--output-format text`. Only `stream-json` is realtime.
The default therefore produces almost nothing while the agent works, and writes
its result at the end.

Measured by logging the output file's size once a second during a single small
task:

```
t= 1s      0 bytes
t= 2s      0 bytes
t= 3s      0 bytes
t= 4s    157 bytes     <- a small early write
t= 5s    157 bytes
   …                    <- nine seconds of complete silence
t=12s    157 bytes
t=13s    848 bytes     <- everything else, then exit
```

**69% of a 13-second task produced no output at all.** Reproduced
independently on a second machine at 71% of a 14-second task. The silent
fraction grows with the length of the task: a lane thinking for an hour can be
silent for nearly all of it.

### What follows from it

A watchdog that kills a lane for producing no output is, in that mode, killing
it for behaving normally. With the old 10-minute default, a lane that worked
for eleven minutes was killed as stalled, requeued into identical silence,
stalled again, exhausted its retries and escalated — spending the whole retry
budget on healthy work and producing an escalation out of a lane that was
fine.

So the stall watchdog is **off by default for lanes whose command sprintd
composes from a prompt**, and on for lanes that supply their own `command`,
which do stream. `--stall`, passed explicitly, turns it on for prompt lanes
too. Deadlines still bound every lane either way.

The eventual fix is to dispatch composed lanes with
`--output-format stream-json`, which makes silence mean what the watchdog
assumes it means. Until then, the safe default is not to guess.

## Reproducing the measurement

```sh
claude -p 'Count from 1 to 15, one number per line, with a short sentence
about each number.' --model claude-haiku-4-5-20251001 > /tmp/streamtest.log 2>&1 &
for i in $(seq 1 25); do
  printf "t=%ss size=%s bytes\n" "$i" "$(wc -c < /tmp/streamtest.log | xargs)"
  sleep 1
done
```

## Conventions

- Table-driven tests with named cases, and a failure message that says what was
  expected and why it matters.
- `go test -race ./...` on anything touching concurrency.
- A test that guards a decision should be checked by breaking the code and
  watching it fail. A guard nobody has seen fail is a guard nobody knows works.
- Real fixtures over fakes wherever the cost is bearable: the git tests build an
  actual bare repository and clone, which is what surfaced both defects above.
