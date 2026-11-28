// The dashboard's words, tested with node (node --test internal/controller/webtest/).
// It sits outside web/ so that it is not served with the dashboard.
import test from "node:test";
import assert from "node:assert/strict";
import { expectText, hostLoad, readyLine } from "../web/js/util.js";

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
