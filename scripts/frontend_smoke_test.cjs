"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

test("classic frontend scripts expose a working datasetOverview helper", () => {
  const context = vm.createContext({});
  const scripts = [
    "web/js/util.js",
    "web/js/api.js",
    "web/js/panel.js",
    "web/js/filters.js",
    "web/js/tables.js",
  ];

  for (const relativePath of scripts) {
    const filename = path.resolve(__dirname, "..", relativePath);
    vm.runInContext(fs.readFileSync(filename, "utf8"), context, {filename});
  }

  assert.equal(vm.runInContext("typeof datasetOverview", context), "function");
  assert.equal(vm.runInContext("typeof renderProblemsButton", context), "function");
  assert.equal(vm.runInContext("typeof renderFmtSeg", context), "function");
  const imeChecks = JSON.parse(vm.runInContext(
    `JSON.stringify({
      nativeComposition: isImeConfirmation({isComposing: true, timeStamp: 100}, null),
      eventBeforeCompositionEnd: isImeConfirmation({isComposing: false, timeStamp: 980}, 1000),
      eventJustAfterCompositionEnd: isImeConfirmation({isComposing: false, timeStamp: 1030}, 1000),
      eventOutsideGuardWindow: isImeConfirmation({isComposing: false, timeStamp: 1051}, 1000),
      ordinaryEnter: isImeConfirmation({isComposing: false, timeStamp: 1051}, null),
    })`,
    context,
  ));
  assert.deepEqual(imeChecks, {
    nativeComposition: true,
    eventBeforeCompositionEnd: true,
    eventJustAfterCompositionEnd: true,
    eventOutsideGuardWindow: false,
    ordinaryEnter: false,
  });


  const rows = [
    {id: "1", fmt: "isuit", type: "normal", misplaced: "误放", date_warn: "日期不一致"},
    {id: "2", fmt: "umi_v3", type: "calibration"},
    {id: "3", fmt: "isuit", type: "normal"},
  ];
  vm.runInContext(
    `S.D = ${JSON.stringify(rows)}; S.datasetETag = '"snapshot-1"';`,
    context,
  );

  const first = JSON.parse(vm.runInContext("JSON.stringify(datasetOverview().stats)", context));
  assert.deepEqual(first, {total: 3, isuit: 2, umi_v3: 1, warn: 1, misplaced: 1});

  vm.runInContext(
    `S.D.push({id: "4", fmt: "umi_v3", type: "normal"}); S.datasetETag = '"snapshot-2"';`,
    context,
  );
  const next = JSON.parse(vm.runInContext("JSON.stringify(datasetOverview().stats)", context));
  assert.equal(next.total, 4);
  assert.equal(next.umi_v3, 2);
});
