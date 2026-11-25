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
  const emptyMessages = JSON.parse(vm.runInContext(
    `JSON.stringify({
      noFilters: uploadRecordsEmptyMessage({}),
      activeStatus: uploadRecordsEmptyMessage({status: "成功"}),
      activeFormat: uploadRecordsEmptyMessage({fmt: "isuit"}),
      whitespaceOnly: uploadRecordsEmptyMessage({q: "   "}),
    })`,
    context,
  ));
  assert.deepEqual(emptyMessages, {
    noFilters: "暂无上传记录，完成上传任务后会显示在这里。",
    activeStatus: "没有符合当前筛选条件的上传记录，请调整条件或点击「清除」。",
    activeFormat: "没有符合当前筛选条件的上传记录，请调整条件或点击「清除」。",
    whitespaceOnly: "暂无上传记录，完成上传任务后会显示在这里。",
  });
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


test("dataset filtering caches results and selection counts stay incremental", () => {
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

  const rows = [
    {
      id: "1", fmt: "isuit", type: "normal", device: "D001", cap_date: "2026-11-24",
      operator: "Bob", task: "Box Sort", sub_task: "A", name: "session-A",
      source: "C:\\data\\isuit", rel: "C:/data/isuit", location: "LA",
      device_sn: "SN001", up_state: "未上传", start: 1, size: 10, duration: 20,
    },
    {
      id: "2", fmt: "umi_v3", type: "normal", device: "D002", cap_date: "2026-11-24",
      operator: "Alice", task: "Medicine Check", sub_task: "B", name: "session-B",
      source: "D:\\data\\umi", rel: "D:/data/umi", location: "LA",
      device_sn: "SN002", up_state: "未上传", start: 2, size: 20, duration: 30,
    },
    {
      id: "3", fmt: "umi_v3", type: "calibration", device: "D002", cap_date: "2026-11-24",
      operator: "Alice", task: "Calibration", sub_task: "C", name: "session-C",
      source: "D:\\data\\calibration", rel: "D:/data/calibration", location: "LA",
      device_sn: "SN002", up_state: "已上传", start: 3, size: 5, duration: 10,
    },
  ];

  vm.runInContext(
    "S.D = " + JSON.stringify(rows) +
      "; S.F = {fmt:'',type:'',device:'',cap_date:'',operator:'',task:'',location:'',status:'',q:''};" +
      "S.datasetETag = '\"snapshot-1\"'; S.sortK = 'start'; S.sortD = -1;",
    context,
  );

  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(rows().map(row => row.id))", context)),
    ["3", "2", "1"],
  );
  assert.equal(vm.runInContext("rows() === rows()", context), true);

  vm.runInContext("firstOverview = datasetOverview()", context);
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(firstOverview.facets)", context)),
    {
      device: ["D001", "D002"],
      cap_date: ["2026-11-24"],
      operator: ["Alice", "Bob"],
    },
  );
  assert.equal(vm.runInContext("datasetOverview() === firstOverview", context), true);
  const firstRows = vm.runInContext("rows()", context);
  vm.runInContext(
    "S.D = S.D.map(row => row.id === '1' ? {...row, device:'D003', operator:'Carol'} : row);",
    context,
  );
  const nextRows = vm.runInContext("rows()", context);
  assert.notEqual(nextRows, firstRows);
  assert.equal(nextRows.find(row => row.id === '1').device, "D003");
  vm.runInContext("nextOverview = datasetOverview()", context);
  assert.notEqual(vm.runInContext("nextOverview", context), vm.runInContext("firstOverview", context));
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(nextOverview.facets)", context)),
    {
      device: ["D002", "D003"],
      cap_date: ["2026-11-24"],
      operator: ["Alice", "Carol"],
    },
  );

  vm.runInContext("S.F.q = " + JSON.stringify("alice medicine"), context);
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(rows().map(row => row.id))", context)),
    ["2"],
  );

  vm.runInContext("S.F.q = " + JSON.stringify("C:\\data"), context);
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(rows().map(row => row.id))", context)),
    ["1"],
  );

  vm.runInContext(
    "S.F = {fmt:'',type:'',device:'',cap_date:'',operator:'',task:'',location:'',status:'',q:''};" +
      "setRowSelected('2', true); setRowSelected('3', true);",
    context,
  );
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(getSelectionStats())", context)),
    {umiNormal: 1, umiCalibration: 1},
  );

  vm.runInContext("setRowSelected('2', false)", context);
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(getSelectionStats())", context)),
    {umiNormal: 0, umiCalibration: 1},
  );

  vm.runInContext("pruneSelection(new Map([['1', true]]))", context);
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(getSelectionStats())", context)),
    {umiNormal: 0, umiCalibration: 0},
  );
  assert.equal(vm.runInContext("S.sel.size", context), 0);

  vm.runInContext("setRowSelected('3', true); clearSelection()", context);
  assert.equal(vm.runInContext("S.sel.size", context), 0);
});


test("log rendering appends rolling updates without rebuilding existing rows", () => {
  const log = {
    children: [],
    scrollTop: 0,
    clientHeight: 100,
    scrollHeight: 0,
    appendCalls: 0,
    replaceCalls: 0,
    get firstElementChild() { return this.children[0] || null; },
    append(fragment) {
      this.appendCalls++;
      this.children.push(...fragment.children);
      this.scrollHeight = this.children.length * 20;
    },
    replaceChildren(fragment) {
      this.replaceCalls++;
      this.children = [...fragment.children];
      this.scrollHeight = this.children.length * 20;
    },
  };
  const onlyErrors = {checked: false};
  const document = {
    getElementById: id => id === "lg" ? log : onlyErrors,
    createDocumentFragment() {
      return {children: [], append(element) { this.children.push(element); }};
    },
    createElement() {
      return {
        className: "",
        textContent: "",
        remove() {
          const index = log.children.indexOf(this);
          if (index >= 0) log.children.splice(index, 1);
          log.scrollHeight = log.children.length * 20;
        },
      };
    },
  };
  const context = vm.createContext({
    document,
    $: id => id === "lg" ? log : onlyErrors,
  });

  for (const relativePath of ["web/js/util.js", "web/js/panel.js"]) {
    const filename = path.resolve(__dirname, "..", relativePath);
    vm.runInContext(fs.readFileSync(filename, "utf8"), context, {filename});
  }

  vm.runInContext('renderLog(["first", "错误"])', context);
  assert.deepEqual(log.children.map(item => item.textContent), ["first", "错误"]);
  assert.equal(log.replaceCalls, 1);

  vm.runInContext("renderLog(S.lastLogSource)", context);
  assert.equal(log.replaceCalls, 1);

  vm.runInContext('renderLog(["错误", "second"])', context);
  assert.deepEqual(log.children.map(item => item.textContent), ["错误", "second"]);
  assert.equal(log.replaceCalls, 1);

  onlyErrors.checked = true;
  vm.runInContext("renderLog(S.lastLogSource)", context);
  assert.deepEqual(log.children.map(item => item.textContent), ["错误"]);
  assert.equal(log.replaceCalls, 2);

  vm.runInContext('renderLog(["second", "失败"])', context);
  assert.deepEqual(log.children.map(item => item.textContent), ["失败"]);
  assert.equal(log.replaceCalls, 2);
});


test("all-rows mode is wired through the table and upload-history API", () => {
  const html = fs.readFileSync(path.resolve(__dirname, "..", "web/index.html"), "utf8");
  const tables = fs.readFileSync(path.resolve(__dirname, "..", "web/js/tables.js"), "utf8");
  const api = fs.readFileSync(path.resolve(__dirname, "..", "umi/api.go"), "utf8");

  assert.ok(html.includes('<option value="all" selected>全部</option>'));
  assert.ok(tables.includes("Number.isFinite(ps) ? S.R.slice(S.page * ps, (S.page + 1) * ps) : S.R"));
  assert.ok(tables.includes("button.hidden = !Number.isFinite(ps)"));
  assert.ok(api.includes('allRows := q.Get("ps") == "all"'));
  assert.ok(api.includes('ORDER BY start_time DESC, id DESC", args...)'));
  assert.ok(api.includes("LIMIT ? OFFSET ?"));
});

test("unchanged table data skips repeated DOM rebuilds", () => {
  const context = vm.createContext({});
  for (const relativePath of ["web/js/util.js", "web/js/filters.js", "web/js/tables.js"]) {
    const filename = path.resolve(__dirname, "..", relativePath);
    vm.runInContext(fs.readFileSync(filename, "utf8"), context, {filename});
  }

  const rows = [{id: "1"}, {id: "2"}];
  vm.runInContext("rowsRef = " + JSON.stringify(rows), context);

  assert.equal(vm.runInContext("shouldRenderTable(1, rowsRef, 0, Infinity, 0)", context), true);
  assert.equal(vm.runInContext("shouldRenderTable(1, rowsRef, 0, Infinity, 0)", context), false);
  assert.equal(vm.runInContext("shouldRenderTable(1, rowsRef, 1, Infinity, 0)", context), true);
  assert.equal(vm.runInContext("shouldRenderTable(1, rowsRef, 1, 100, 0)", context), true);
  assert.equal(vm.runInContext("shouldRenderTable(1, rowsRef, 1, 100, 1)", context), true);
  assert.equal(vm.runInContext("shouldRenderTable(2, rowsRef, 1, 100, 1)", context), true);
  assert.equal(vm.runInContext("shouldRenderTable(2, rowsRef, 1, 100, 1)", context), false);
});
