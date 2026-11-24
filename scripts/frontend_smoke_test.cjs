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
      nativeComposition: isImeConfirmation({isComposing: true, timeStamp: 100}, false, null),
      trackedComposition: isImeConfirmation({isComposing: false, timeStamp: 100}, true, null),
      eventBeforeCompositionEnd: isImeConfirmation({isComposing: false, timeStamp: 980}, false, 1000),
      eventJustAfterCompositionEnd: isImeConfirmation({isComposing: false, timeStamp: 1030}, false, 1000),
      eventOutsideGuardWindow: isImeConfirmation({isComposing: false, timeStamp: 1051}, false, 1000),
      ordinaryEnter: isImeConfirmation({isComposing: false, timeStamp: 1051}, false, null),
    })`,
    context,
  ));
  assert.deepEqual(imeChecks, {
    nativeComposition: true,
    trackedComposition: true,
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


test("dashboard and configuration controls have programmatic accessible names", () => {
  const html = fs.readFileSync(path.resolve(__dirname, "..", "web/index.html"), "utf8");
  const labeledControls = [
    ["c_storage_type", "存储类型"],
    ["c_endpoint", "Endpoint"],
    ["c_ak", "Access Key ID"],
    ["c_sk", "Access Key Secret"],
    ["c_bucket", "ISuit-V3 桶"],
    ["c_bucket_umi", "UMI_v3 桶"],
    ["c_ds_jobs", "数据集并发"],
    ["c_jobs", "文件并发"],
    ["c_parallel", "分片并发"],
    ["c_wan", "网络出口数（WAN）"],
    ["c_target_mbps", "目标总上传带宽（Mbps，0 = 自动探索最大吞吐）"],
    ["c_auto_tune", "自动调优"],
    ["c_max_jobs", "最大文件并发"],
    ["c_max_parallel", "最大分片并发"],
    ["c_net_wait", "断网自动等待（分钟，0 = 不等待）"],
  ];

  for (const [id, label] of labeledControls) {
    assert.match(html, new RegExp(`<label\\s+for="${id}">${label}<\\/label>`));
    assert.match(html, new RegExp(`<(?:input|select)\\b[^>]*\\bid="${id}"[^>]*>`));
  }

  const ariaLabeledControls = [
    ["input", "city", "城市缩写"],
    ["input", "room", "房间号"],
    ["select", "ft", "数据类型筛选"],
    ["select", "fs", "状态筛选"],
    ["select", "fd", "设备筛选"],
    ["select", "fe", "采集日期筛选"],
    ["select", "fo", "人员筛选"],
    ["input", "fk", "任务名筛选"],
    ["input", "fc", "地点筛选"],
    ["input", "q", "关键字筛选"],
    ["select", "ps", "每页条数"],
    ["select", "vsel", "选择相机视频"],
    ["select", "c_profile_select", "配置方案"],
    ["input", "c_profile_name", "新配置名称"],
  ];

  for (const [tagName, id, label] of ariaLabeledControls) {
    const control = html.match(new RegExp(`<${tagName}\\b[^>]*\\bid="${id}"[^>]*>`));
    assert.ok(control, `Missing ${tagName}#${id}`);
    assert.match(control[0], new RegExp(`\\baria-label="${label}"`));
  }

  assert.match(html, /<button\b[^>]*aria-label="上一页"[^>]*data-act="page"/);
  assert.match(html, /<button\b[^>]*aria-label="下一页"[^>]*data-act="page"/);
  assert.match(html, /<div class="tabs" role="group" aria-label="页面视图">/);
  assert.match(html, /<div class="seg" role="group" aria-label="上传顺序">/);
});


test("modal dialogs expose names and manage keyboard focus", () => {
  const html = fs.readFileSync(path.resolve(__dirname, "..", "web/index.html"), "utf8");
  const dialogs = fs.readFileSync(path.resolve(__dirname, "..", "web/js/dialogs.js"), "utf8");
  const modalNames = [
    ["mb", "mbTitle"],
    ["mg", "mgTitle"],
    ["mm", "mmTitle"],
    ["vm", "vtitle"],
    ["mp", "mpTitle"],
    ["mc", "mcTitle"],
    ["bye", "byeTitle"],
  ];

  for (const [modalId, titleId] of modalNames) {
    assert.match(html, new RegExp('<div class="modal[^"]*" id="' + modalId + '" role="dialog" aria-modal="true" aria-labelledby="' + titleId + '">'));
    assert.match(html, new RegExp('<h3 id="' + titleId + '">[^<]+</h3>'));
  }

  let activeElement;
  const trigger = {
    isConnected: true,
    focus() { activeElement = this; },
  };
  const first = {
    hidden: false,
    getClientRects: () => [1],
    focus() { activeElement = this; },
  };
  const last = {
    hidden: false,
    getClientRects: () => [1],
    focus() { activeElement = this; },
  };
  let context;
  const dialog = {
    hasAttribute: () => false,
    querySelectorAll: () => [first, last],
    contains: element => element === first || element === last,
    focus() { activeElement = this; },
  };
  const closeButton = {click: () => context.closeModal("test")};
  const modal = {
    style: {display: "none"},
    classList: {contains: () => false},
    querySelector: selector => selector === ".dlg" ? dialog : closeButton,
  };
  const listeners = {};
  const document = {
    get activeElement() { return activeElement; },
    addEventListener(type, callback) { listeners[type] = callback; },
    querySelectorAll: () => [modal],
  };
  activeElement = trigger;
  context = vm.createContext({document, $: id => id === "test" ? modal : {addEventListener() {}}});
  vm.runInContext(dialogs, context, {filename: "web/js/dialogs.js"});

  context.openModal("test");
  assert.equal(modal.style.display, "flex");
  assert.equal(activeElement, first);

  activeElement = last;
  let prevented = false;
  listeners.keydown({key: "Tab", shiftKey: false, preventDefault() { prevented = true; }});
  assert.equal(prevented, true);
  assert.equal(activeElement, first);

  prevented = false;
  listeners.keydown({key: "Tab", shiftKey: true, preventDefault() { prevented = true; }});
  assert.equal(prevented, true);
  assert.equal(activeElement, last);

  listeners.keydown({key: "Escape", preventDefault() {}});
  assert.equal(modal.style.display, "none");
  assert.equal(activeElement, trigger);
});


test("data table sorting and selection controls are keyboard and screen-reader accessible", () => {
  const tables = fs.readFileSync(path.resolve(__dirname, "..", "web/js/tables.js"), "utf8");
  const main = fs.readFileSync(path.resolve(__dirname, "..", "web/js/main.js"), "utf8");

  assert.ok(tables.includes('<caption class="sr-only">数据集列表。可排序列可通过列标题按钮排序；再次操作可切换排序方向。</caption>'));
  assert.ok(tables.includes('<button type="button" class="th-sort" data-act="sort"'));
  assert.ok(tables.includes('aria-label="按${esc(c[0])}排序"'));
  assert.ok(tables.includes('aria-sort="${direction}"'));
  assert.ok(tables.includes('<span aria-hidden="true">${sorted ?'));
  assert.ok(!tables.includes('tabindex="0"'));
  assert.ok(tables.includes('aria-label="全选当前筛选结果"'));
  assert.ok(tables.includes('aria-label="选择会话 ${esc(r.name)}"'));

  assert.ok(main.includes("button.th-sort"));
  assert.ok(main.includes("event.detail === 0"));
  assert.ok(!main.includes("$('tb').addEventListener('keydown'"));
});


test("directory navigation and root removal use named native buttons", () => {
  const panel = fs.readFileSync(path.resolve(__dirname, "..", "web/js/panel.js"), "utf8");
  const dialogs = fs.readFileSync(path.resolve(__dirname, "..", "web/js/dialogs.js"), "utf8");
  const css = fs.readFileSync(path.resolve(__dirname, "..", "web/css/app.css"), "utf8");

  assert.ok(panel.includes('<button type="button" class="root-remove"'));
  assert.ok(panel.includes('aria-label="移除目录 ${esc(r)}"'));
  assert.ok(panel.includes('data-act="rmRoot"'));
  assert.ok(!panel.includes('<i data-act="rmRoot"'));

  assert.ok(dialogs.includes('<button type="button" class="dir-enter"'));
  assert.ok(dialogs.includes('aria-label="进入上一级目录"'));
  assert.ok(dialogs.includes('aria-label="进入目录 ${esc(dir.name)}"'));
  assert.ok(dialogs.includes('data-act="enter"'));
  assert.ok(!dialogs.includes('<span data-act="enter"'));

  assert.ok(css.includes('.chip .root-remove:focus-visible') || css.includes('button:focus-visible'));
  assert.ok(css.includes('.dirlist button.dir-enter'));
  assert.ok(css.includes('.dirlist .dir-check-spacer'));
});