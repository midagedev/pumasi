// The dashboard's words, tested with node (node --test internal/controller/webtest/).
// It sits outside web/ so that it is not served with the dashboard.
import test from "node:test";
import assert from "node:assert/strict";
import { expectText, expressRow, hostLoad, readyLine, shownRatio, stepOf, machineShare, cardGB, moreProblems, attentionWords, aheadJobs, aheadWarm } from "../web/js/util.js";

const GiB = 1024 ** 3;

// The same cases as TestExpectText (demo_test.go): the controller and the
// dashboard say a forecast in the same words.
test("expectText says which job a wait is for, as the controller does", () => {
  const R = [{ name: "r1", host: "h1", job: "e2e", ageSeconds: 200 }, { name: "r2", host: "h1", job: "e2e", ageSeconds: 70 }, { name: "r3", host: "h1", job: "build" }, { name: "r4", host: "h1", job: "pack" }];
  for (const [e, self, want, wait, rs] of [
    [{ inSeconds: 40, host: "h1" }, "lint", "starts in under a minute on h1", "", R],
    [{ inSeconds: 150, host: "h1", after: "e2e", afterRunner: "r1" }, "lint", "starts in ~3m on h1, when e2e ends", "", R.slice(0, 1)],
    [{ inSeconds: 6000, host: "h2", after: "build", afterRunner: "r3" }, "lint", "starts in ~1h40m on h2, when build on h1 ends", "", R],
    [{ inSeconds: 30, host: "h1", after: "pack", afterRunner: "r4" }, "pack", "starts in under a minute on h1, when another pack ends", "", R],
    [{ inSeconds: 150, host: "h1", after: "lint" }, "test", "starts in ~3m on h1, after the lint waiting ahead of it", "", R],
    [{ inSeconds: 30, host: "h1", after: "e2e", afterRunner: "r1" }, "lint", "starts in under a minute on h1, when the e2e running there for 3m ends", "e2e", R],
    [{ inSeconds: 30, host: "h1", after: "e2e", afterRunner: "r2" }, "lint", "starts in under a minute on h1, when the e2e running there for 1m ends", "e2e", R],
    [{ inSeconds: 30, host: "h2", after: "build", afterRunner: "r3" }, "lint", "starts in under a minute on h2, when the build running on h1 ends", "build", R],
    [{ inSeconds: 150, host: "h1", after: "pack", afterRunner: "r4", afterWith: 2 }, "lint", "starts in ~3m on h1, when pack ends, once 2 others there have ended too", "", R],
    [{ inSeconds: 150, host: "h1", after: "e2e", afterRunner: "r2", afterWith: 1 }, "lint", "starts in ~3m on h1, when the e2e running there for 1m ends, once 1 other there has ended too", "", R],
  ]) assert.equal(expectText(e, rs, self, wait ? [{ job: wait }] : []), want);
});

// A host's ready runners are counted the same in its word, its line in the
// hosts list and the header (every runner up with no job), whether or not
// each gives way.
test("a host's ready runners are all counted, those that give way among them", () => {
  const h = { name: "h1", reachable: true, memBytes: 64 * GiB, cpus: 16, freeMem: 2 * GiB, freeCpus: 1, runners: 4 };
  const run = (o) => ({ host: "h1", state: "idle", estMem: 8 * GiB, estCpus: 2, ...o });
  const rs = [run({ job: "e2e", state: "busy", estMem: 40 * GiB, estCpus: 10 }), run({ givesWay: true }), run({ givesWay: true }), run({})];
  const L = hostLoad(h, rs);
  assert.equal(L.word, "3 ready");
  assert.equal(L.ready, 3);
  assert.equal(L.giving, 2);
  assert.match(readyLine(L), /^3 ready; 2 give way: /);
  assert.match(readyLine(hostLoad(h, rs.slice(0, 3))), /^2 ready runners give way: /);
  assert.equal(readyLine(hostLoad(h, [rs[0], rs[3]])), "1 ready runner keeps its room");
});

// The room kept on the map is the expedited job's, not that of a job ahead of
// it in its class that it pulls along (also "expedite" in the line).
test("expressRow picks the job expedited by hand", () => {
  const p = {
    expedited: [{ job: "j9", githubJobId: 9, position: 3 }],
    line: [
      { job: "j1", level: "normal", position: 1 },
      { job: "j7", level: "expedite", position: 2, why: "ahead of j9 in its class" },
      { job: "j9", githubJobId: 9, level: "expedite", position: 3, waitingFor: "h1" },
    ],
  };
  assert.equal(expressRow(p).job, "j9");
  assert.equal(expressRow({ ...p, expedited: [{ job: "j9", position: 0 }] }), null);
});

// A host as full as that only by runners with no job (ready ones keeping
// their room, starting ones) runs nothing: it is HELD, not BUSY or FULL.
test("a host whose room is held by runners with no job is held, not busy", () => {
  const h = { name: "h1", reachable: true, memBytes: 32 * GiB, cpus: 16, freeMem: 6 * GiB, freeCpus: 10, runners: 2 };
  const idle = { host: "h1", state: "idle", estMem: 12 * GiB, estCpus: 2 };
  const starting = { host: "h1", state: "starting", estMem: 12 * GiB, estCpus: 2 };
  assert.equal(hostLoad(h, [idle, starting]).word, "held");
  // with a job running, the same room is busy
  assert.equal(hostLoad(h, [{ ...idle, job: "e2e", state: "busy" }, starting]).word, "busy");
});

// The CI panel's percent is what the two values beside it make as written.
test("shownRatio uses the values as the panel writes them", () => {
  assert.equal(Math.round(shownRatio("time", 86.4, 121.6) * 100), 42);
  assert.equal(Math.round(shownRatio("memory", 8.48 * GiB, 11.53 * GiB) * 100), 35);
  assert.ok(isNaN(shownRatio("time", 0, 10)));
});

// A job that got faster names the step that shrank most, not the first one.
test("stepOf names the step that moved most the way the job did", () => {
  const steps = [{ name: "Set up job", before: 3, after: 2 }, { name: "Install", before: 25, after: 20 }, { name: "Typecheck", before: 47.5, after: 37 }, { name: "Lint", before: 5, after: 6 }];
  assert.equal(stepOf({ ratio: -0.22, steps }).name, "Typecheck");
  assert.equal(stepOf({ ratio: 0.1, steps }).name, "Lint");
  assert.equal(stepOf({ ratio: 0.1, steps: [] }), null);
});

// held + machine + free on a host's card add up to its memory as written:
// 128 GB, held 105.7 (reads 106), free 3.62 (reads 3.6): machine 18.4; and
// 32 GB, held 25.7, free 6.3: no machine share, and the two read 25.7 + 6.3.
test("a host card's machine share makes held + machine + free its memory", () => {
  const M = { lend: 128 * GiB, held: 80 * GiB, kept: 12.7 * GiB, give: 13 * GiB, free: 3.62 * GiB };
  assert.equal(machineShare(M), 18.4);
  assert.equal(machineShare({ lend: 24 * GiB, held: 16 * GiB, kept: 0, give: 0, free: 8 * GiB }), 0);
  assert.equal(machineShare({ lend: 32 * GiB, held: 25.7 * GiB, kept: 0, give: 0, free: 6.3 * GiB }), 0);
  assert.deepEqual([cardGB(25.7), cardGB(26), cardGB(0.66), cardGB(105.7), cardGB(99.96)], ["25.7", "26", "0.7", "106", "100"]);
});

// One meaning for "problem": the header's count, the alarm board and the List's
// problem cards are the same findings; the List's other cards are classes
// waiting and notes, as /api/explain's levels.
test("a problem is a problem finding, and the List counts waits and notes apart", () => {
  const incidents = [{ kind: "ci-too-slow", subject: "unit-tests" }];
  const fs = [{ level: "problem", kind: "ci-too-slow", subject: "unit-tests" }, { level: "problem", kind: "host", subject: "linux-spare" },
    { level: "problem", kind: "warning", subject: "credentials" }, { level: "wait", kind: "waiting", subject: "e2e" }, { level: "note", kind: "overflow", subject: "light" }];
  assert.deepEqual(moreProblems(incidents, fs).map((f) => f.subject), ["linux-spare"]);
  assert.equal(attentionWords(fs), "3 problems · 1 class waiting · 1 note");
  assert.equal(attentionWords(fs.slice(0, 1)), "1 problem");
  assert.equal(attentionWords([]), "");
});

// Two release runners started ahead and a pr-checks one kept warm make the
// host's 3 READY: the ahead tag says all three.
test("an ahead tag counts the host's runners kept warm", () => {
  const ah = (name) => ({ name, host: "a", class: "release", state: "idle", ahead: "package expected in ~5s, when release-check ends" });
  const rs = [ah("r1"), ah("r2"), { name: "w", host: "a", class: "pr-checks", state: "idle" }, { name: "x", host: "b", class: "pr-checks", state: "idle" }];
  assert.equal(aheadJobs(rs, rs[0]), "a release job (one of 2 runners)");
  assert.equal(aheadJobs(rs.slice(1), rs[1]), "package");
  assert.equal(aheadWarm(rs, rs[0]), "plus 1 kept warm");
  assert.equal(aheadWarm(rs.slice(0, 2), rs[0]), "");
  // one of the two still starting: the tag says the one that listens, so 1 + 3 warm make 4 READY
  const st = [{ ...ah("r1"), state: "starting" }, ah("r2"), ...["p", "d1", "d2"].map((name) => ({ name, host: "a", class: "deploy", state: "idle" }))];
  assert.equal(aheadJobs(st, st[1]), "package");
  assert.equal(aheadWarm(st, st[1]), "plus 3 kept warm");
});
