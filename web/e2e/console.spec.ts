import { expect, Page, test } from "@playwright/test";

type Role = "admin" | "viewer";
type Language = "zh" | "en";
type Theme = "dark" | "light";

const matrix: Array<{ role: Role; language: Language; theme: Theme }> = [];
for (const role of ["admin", "viewer"] as const) {
  for (const language of ["zh", "en"] as const) {
    for (const theme of ["dark", "light"] as const) matrix.push({ role, language, theme });
  }
}

for (const scenario of matrix) {
  test(`${scenario.role} ${scenario.language} ${scenario.theme} workspace`, async ({ page }) => {
    await page.addInitScript(({ language, theme }) => {
      localStorage.setItem("xirang.language", language);
      localStorage.setItem("xirang.theme", theme);
      localStorage.setItem("xirang.refreshSeconds", "60");
    }, scenario);
    await mockControlPlane(page, scenario.role);
    await page.goto("/");

    await page.getByRole("button", { name: scenario.language === "zh" ? "令牌" : "Token" }).click();
    await page.getByLabel("Bootstrap / API / OIDC Token").fill(`${scenario.role}-token`);
    await page.getByRole("button", { name: scenario.language === "zh" ? "连接控制平面" : "Connect to control plane" }).click();
    await expect(page.getByRole("heading", { level: 1, name: scenario.language === "zh" ? "集群运行总览" : "Cluster overview" })).toBeVisible();

    await expect(page.locator("html")).toHaveAttribute("data-theme", scenario.theme);
    await expect(page.locator("html")).toHaveAttribute("lang", scenario.language === "zh" ? "zh-CN" : "en");
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);

    const terminalLabel = scenario.language === "zh" ? "终端连接" : "Terminal access";
    const enrollmentLabel = scenario.language === "zh" ? "节点接入" : "Node enrollment";
    const queueLabel = scenario.language === "zh" ? "队列策略" : "Queue policies";
    if (scenario.role === "admin") {
      await expect(page.getByRole("button", { name: terminalLabel })).toBeVisible();
      await expect(page.getByRole("button", { name: enrollmentLabel })).toBeVisible();
      await page.getByRole("button", { name: terminalLabel }).click();
      await expect(page).toHaveURL(/\/terminal$/);
      await expect(page.getByText("Agent mTLS · PTY · 10 min idle · 30 min max")).toBeVisible();
    } else {
      await expect(page.getByRole("button", { name: terminalLabel })).toHaveCount(0);
      await expect(page.getByRole("button", { name: enrollmentLabel })).toHaveCount(0);
      await expect(page.getByRole("button", { name: queueLabel })).toHaveCount(0);
      await page.evaluate(() => {
        window.history.pushState({}, "", "/terminal");
        window.dispatchEvent(new PopStateEvent("popstate"));
      });
      await expect(page).toHaveURL("/");
    }

    await page.getByRole("button", { name: scenario.language === "zh" ? "集群概览" : "Cluster overview" }).click();
    await expect(page).toHaveURL("/");
    await expect(page.locator(".app-shell")).toHaveScreenshot(`${scenario.role}-${scenario.language}-${scenario.theme}.png`, {
      mask: [page.locator(".live-indicator"), page.locator(".sidebar-footer small")],
    });
  });
}

test("regular user shared files are presented as an isolated workspace", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("xirang.language", "en");
    localStorage.setItem("xirang.theme", "light");
    localStorage.setItem("xirang.refreshSeconds", "60");
  });
  await mockControlPlane(page, "viewer");
  await page.goto("/");
  await page.getByRole("button", { name: "Token" }).click();
  await page.getByLabel("Bootstrap / API / OIDC Token").fill("viewer-token");
  await page.getByRole("button", { name: "Connect to control plane" }).click();
  await page.getByRole("button", { name: "Shared storage" }).click();
  await expect(page).toHaveURL(/\/storage$/);
  await expect(page.getByText("Only your files are visible and manageable")).toBeVisible();
  await expect(page.getByText("private-from-another-user.dat")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Upload folder" })).toBeVisible();
  await page.getByPlaceholder("Filter current folder").fill("input");
  await expect(page.getByText("input.dat", { exact: true })).toBeVisible();
  await expect(page.getByText("results", { exact: true })).toHaveCount(0);
  await expect(page.locator(".storage-form")).toHaveCount(0);
});

test("administrator template opens as structured fields and can switch to raw script", async ({ page }) => {
  await loginAsAdmin(page);
  await page.getByRole("button", { name: "Job scheduling" }).click();
  await page.getByRole("button", { name: "Load and edit" }).click();
  await expect(page.getByLabel("Architecture")).toHaveValue("arm64");
  await expect(page.getByLabel("Logical CPU cores")).toHaveValue("4");
  await expect(page.getByLabel("Memory (for example 4G)")).toHaveValue("8G");
  const raw = page.getByLabel("Show and directly edit the raw template script");
  await expect(raw).not.toBeChecked();
  await raw.check();
  await expect(page.getByLabel("XIRANG job script")).toContainText("architecture = arm64");
  await expect(page.getByLabel("Architecture")).toHaveCount(0);
});

test("native build exposes shared paths, cross compilation, and eligible workers", async ({ page }) => {
  await loginAsAdmin(page);
  await page.getByRole("button", { name: "Native builds" }).click();
  await expect(page.getByRole("heading", { name: "Build nodes and toolchain capabilities" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Historical builds" })).toBeVisible();
  await page.getByRole("button", { name: "New build" }).click();
  await page.getByLabel("Cross-compile").check();
  await expect(page.getByLabel("Builder architecture")).toBeVisible();
  await expect(page.getByLabel("Build node")).toBeVisible();
  await page.getByRole("button", { name: "Source relative path" }).click();
  await expect(page.getByRole("heading", { name: "Select folder" })).toBeVisible();
  await expect(page.getByRole("option", { name: /Research/ })).toHaveCount(1);
});

test("identity management presents complete editable group lifecycle", async ({ page }) => {
  await loginAsAdmin(page);
  await page.getByRole("button", { name: "Identity management" }).click();
  await expect(page.getByRole("heading", { name: "User group management" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "User-group runtime policy" })).toHaveCount(0);
  const defaultGroup = page.getByLabel("Default group for new accounts");
  await expect(defaultGroup).toHaveValue("");
  await defaultGroup.selectOption("group_userlabs");
  await page.getByRole("button", { name: "Save default" }).click();
  await expect(page.getByRole("button", { name: "Default saved" })).toBeDisabled();
  const card = page.locator(".group-policy-grid article").filter({ hasText: "unlimited-lab" });
  await expect(card.getByLabel("CPU cores (0 unlimited)")).toHaveValue("0");
  await expect(card.getByLabel("Memory G (0 unlimited)")).toHaveValue("0");
  await expect(card.getByRole("button", { name: "Save changes" })).toBeVisible();
  await expect(card.getByRole("button", { name: "Disable" })).toBeVisible();
  await expect(card.getByRole("button", { name: "Remove" })).toBeVisible();

  const account = page.locator(".local-account-row").filter({ hasText: "E2E Administrator" });
  const unlimited = account.getByRole("checkbox", { name: /unlimited-lab/ });
  const userLabs = account.getByRole("checkbox", { name: /UserLabs/ });
  await expect(unlimited).not.toBeChecked();
  await expect(userLabs).toBeChecked();
  await unlimited.check();
  await expect(account.getByRole("button", { name: "Save groups" })).toBeEnabled();
  await account.getByRole("button", { name: "Save groups" }).click();
  await expect(account.getByRole("button", { name: "Groups saved" })).toBeDisabled();

  const assignedCard = page.locator(".group-policy-grid article").filter({ hasText: "UserLabs" });
  page.once("dialog", async (dialog) => {
    expect(dialog.message()).toContain("detached from 1 account");
    await dialog.accept();
  });
  await assignedCard.getByRole("button", { name: "Remove" }).click();
  await expect(page.locator(".group-policy-grid article").filter({ hasText: "UserLabs" })).toHaveCount(0);
  await expect(account.getByRole("checkbox", { name: /unlimited-lab/ })).toBeChecked();
  await expect(defaultGroup).toHaveValue("");
});

test("overview uses the topology bidirectional rate and the product title", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("xirang.language", "en");
    localStorage.setItem("xirang.theme", "dark");
    localStorage.setItem("xirang.refreshSeconds", "60");
  });
  await mockControlPlane(page, "admin");
  const nodes = [
    { id: "node-a", name: "node-a", architecture: "amd64", status: "ready", roles: ["management", "compute"], capacity: { milliCpu: 8000, memoryBytes: 16_000_000_000 }, createdAt: "2026-08-02T00:00:00Z", lastSeenAt: "2026-08-02T00:00:01Z" },
    { id: "node-b", name: "node-b", architecture: "arm64", status: "ready", roles: ["compute"], capacity: { milliCpu: 8000, memoryBytes: 16_000_000_000 }, createdAt: "2026-08-02T00:00:00Z", lastSeenAt: "2026-08-02T00:00:01Z" },
  ];
  await page.route("**/api/v1/nodes", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: nodes }) }));
  await page.route("**/api/v1/nodes/*/inventory", (route) => {
    const nodeID = new URL(route.request().url()).pathname.split("/")[4];
    return route.fulfill({ contentType: "application/json", body: JSON.stringify({ networkInterfaces: [{ name: "eth0", addresses: [nodeID === "node-a" ? "10.0.0.1/24" : "10.0.0.2/24"], speedBitsPerSecond: 25_000_000_000 }] }) });
  });
  await page.route("**/api/v1/nodes/*/observations?*", (route) => {
    const nodeID = new URL(route.request().url()).pathname.split("/")[4];
    const current = nodeID === "node-a" ? { receivedBytes: 1000, transmittedBytes: 2000 } : { receivedBytes: 1500, transmittedBytes: 800 };
    return route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: [
      { observedAt: "2026-08-02T00:00:01Z", usedMilliCpu: 1000, usedMemoryBytes: 1000, receivedBytes: current.receivedBytes, transmittedBytes: current.transmittedBytes, interfaceCounters: { eth0: current } },
      { observedAt: "2026-08-02T00:00:00Z", usedMilliCpu: 1000, usedMemoryBytes: 1000, receivedBytes: 0, transmittedBytes: 0, interfaceCounters: { eth0: { receivedBytes: 0, transmittedBytes: 0 } } },
    ] }) });
  });
  await page.route("**/api/v1/topology", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({
    version: 1, generatedAt: "2026-08-02T00:00:01Z", reportCount: 2, vertices: [], edges: [], pathSelections: [], routes: [], neighbors: [],
    pathProbes: [{ nodeId: "node-a", targetNodeId: "node-b", sourceInterface: "eth0", sourceAddress: "10.0.0.1", targetInterface: "eth0", targetAddress: "10.0.0.2", sourceSpeedBitsPerSecond: 25_000_000_000, targetSpeedBitsPerSecond: 25_000_000_000, latencyMicros: 100, reachable: true, observedAt: "2026-08-02T00:00:01Z" }],
  }) }));

  await page.goto("/");
  await expect(page).toHaveTitle("XIRANG System");
  await page.getByRole("button", { name: "Token" }).click();
  await page.getByLabel("Bootstrap / API / OIDC Token").fill("admin-token");
  await page.getByRole("button", { name: "Connect to control plane" }).click();

  const overviewNetwork = page.locator(".metric-card").filter({ hasText: "Network activity" });
  await expect(overviewNetwork.locator("strong")).toHaveText("18 Kbit/s");
  await expect(overviewNetwork).toContainText("Aggregate bidirectional rate across 1 node links");
  await page.getByRole("button", { name: "Network topology" }).click();
  const topologyRate = page.locator(".topology-metrics > div").filter({ hasText: "Aggregate bidirectional rate" });
  await expect(topologyRate.locator("strong")).toHaveText("18 Kbit/s");
});

test("unavailable nodes do not expose stale link rates or fallback claims", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("xirang.language", "en");
    localStorage.setItem("xirang.theme", "dark");
    localStorage.setItem("xirang.refreshSeconds", "60");
  });
  await mockControlPlane(page, "admin");
  const nodes = [
    { id: "node-a", name: "controller", architecture: "amd64", status: "ready", roles: ["management", "compute"], capacity: { milliCpu: 8000, memoryBytes: 16_000_000_000 }, createdAt: "2026-08-02T00:00:00Z", lastSeenAt: "2026-08-02T00:00:10Z" },
    { id: "node-b", name: "offline-worker", architecture: "arm64", status: "offline", roles: ["compute"], capacity: { milliCpu: 8000, memoryBytes: 16_000_000_000 }, createdAt: "2026-08-02T00:00:00Z", lastSeenAt: "2026-08-01T00:00:00Z" },
  ];
  await page.route("**/api/v1/nodes", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: nodes }) }));
  await page.route("**/api/v1/nodes/*/inventory", (route) => {
    const nodeID = new URL(route.request().url()).pathname.split("/")[4];
    const suffix = nodeID === "node-a" ? "1" : "2";
    return route.fulfill({ contentType: "application/json", body: JSON.stringify({ networkInterfaces: [
      { name: "eth0", addresses: [`10.0.0.${suffix}/24`], speedBitsPerSecond: 10_000_000_000 },
      { name: "eth1", addresses: [`10.1.0.${suffix}/24`], speedBitsPerSecond: 1_000_000_000 },
    ] }) });
  });
  await page.route("**/api/v1/nodes/*/observations?*", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: [
    { observedAt: "2026-08-02T00:00:01Z", interfaceCounters: { eth0: { receivedBytes: 2000, transmittedBytes: 3000 }, eth1: { receivedBytes: 4000, transmittedBytes: 5000 } } },
    { observedAt: "2026-08-02T00:00:00Z", interfaceCounters: { eth0: { receivedBytes: 1000, transmittedBytes: 1000 }, eth1: { receivedBytes: 1000, transmittedBytes: 1000 } } },
  ] }) }));
  await page.route("**/api/v1/topology", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({
    version: 1, generatedAt: "2026-08-02T00:00:01Z", reportCount: 2, vertices: [], edges: [], pathSelections: [], routes: [], neighbors: [],
    pathProbes: [
      { nodeId: "node-a", targetNodeId: "node-b", sourceInterface: "eth0", sourceAddress: "10.0.0.1", targetInterface: "eth0", targetAddress: "10.0.0.2", sourceSpeedBitsPerSecond: 10_000_000_000, targetSpeedBitsPerSecond: 10_000_000_000, latencyMicros: 100, reachable: true, observedAt: "2026-08-02T00:00:01Z" },
      { nodeId: "node-a", targetNodeId: "node-b", sourceInterface: "eth1", sourceAddress: "10.1.0.1", targetInterface: "eth1", targetAddress: "10.1.0.2", sourceSpeedBitsPerSecond: 1_000_000_000, targetSpeedBitsPerSecond: 1_000_000_000, latencyMicros: 200, reachable: true, observedAt: "2026-08-02T00:00:01Z" },
    ],
  }) }));

  await page.goto("/");
  await page.getByRole("button", { name: "Token" }).click();
  await page.getByLabel("Bootstrap / API / OIDC Token").fill("admin-token");
  await page.getByRole("button", { name: "Connect to control plane" }).click();
  await page.getByRole("button", { name: "Network topology" }).click();

  await expect(page.locator(".graph-link-rate")).toHaveText("↑ Unavailable · ↓ Unavailable");
  // The separator is deliberate: "Negotiated Unavailable" ran a field name
  // into its state as though the two were one phrase.
  await expect(page.locator(".graph-link-capacity")).toHaveText("Negotiated · Unavailable");
  await expect(page.locator(".graph-link-fallback")).toHaveCount(0);
  await expect(page.locator(".topology-metrics > div").filter({ hasText: "Aggregate bidirectional rate" }).locator("strong")).toHaveText("0 bit/s");
  await expect(page.locator(".managed-link-list")).toContainText("Node unavailable; fallback status unknown");
  await expect(page.getByText(/fallback links? available/)).toHaveCount(0);
});


// "1 fallback links available" is the plural falling out of a template string,
// and it is the kind of defect that reappears the moment the sentence is
// rewritten. Two interfaces give one primary and one fallback; three give two.
for (const { interfaces, expected } of [
  { interfaces: 2, expected: "1 fallback link available" },
  { interfaces: 3, expected: "2 fallback links available" },
]) {
  test(`a link with ${interfaces - 1} fallback interface(s) is counted in the right plural`, async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("xirang.language", "en");
      localStorage.setItem("xirang.theme", "dark");
      localStorage.setItem("xirang.refreshSeconds", "60");
    });
    await mockControlPlane(page, "admin");
    const names = ["eth0", "eth1", "eth2"].slice(0, interfaces);
    await page.route("**/api/v1/nodes", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: [
      { id: "node-a", name: "controller", architecture: "amd64", status: "ready", roles: ["management", "compute"], capacity: { milliCpu: 8000, memoryBytes: 16_000_000_000 }, createdAt: "2026-08-02T00:00:00Z", lastSeenAt: "2026-08-02T00:00:10Z" },
      { id: "node-b", name: "worker", architecture: "arm64", status: "ready", roles: ["compute"], capacity: { milliCpu: 8000, memoryBytes: 16_000_000_000 }, createdAt: "2026-08-02T00:00:00Z", lastSeenAt: "2026-08-02T00:00:10Z" },
    ] }) }));
    await page.route("**/api/v1/nodes/*/inventory", (route) => {
      const suffix = new URL(route.request().url()).pathname.split("/")[4] === "node-a" ? "1" : "2";
      return route.fulfill({ contentType: "application/json", body: JSON.stringify({
        networkInterfaces: names.map((name, index) => ({ name, addresses: [`10.${index}.0.${suffix}/24`], speedBitsPerSecond: 10_000_000_000 - index })),
      }) });
    });
    await page.route("**/api/v1/nodes/*/observations?*", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: [
      { observedAt: "2026-08-02T00:00:01Z", interfaceCounters: Object.fromEntries(names.map((name) => [name, { receivedBytes: 2000, transmittedBytes: 3000 }])) },
      { observedAt: "2026-08-02T00:00:00Z", interfaceCounters: Object.fromEntries(names.map((name) => [name, { receivedBytes: 1000, transmittedBytes: 1000 }])) },
    ] }) }));
    await page.route("**/api/v1/topology", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({
      version: 1, generatedAt: "2026-08-02T00:00:01Z", reportCount: 2, vertices: [], edges: [], pathSelections: [], routes: [], neighbors: [],
      pathProbes: names.map((name, index) => ({
        nodeId: "node-a", targetNodeId: "node-b", sourceInterface: name, sourceAddress: `10.${index}.0.1`,
        targetInterface: name, targetAddress: `10.${index}.0.2`,
        sourceSpeedBitsPerSecond: 10_000_000_000 - index, targetSpeedBitsPerSecond: 10_000_000_000 - index,
        latencyMicros: 100 + index, reachable: true, observedAt: "2026-08-02T00:00:01Z",
      })),
    }) }));

    await page.goto("/");
    await page.getByRole("button", { name: "Token" }).click();
    await page.getByLabel("Bootstrap / API / OIDC Token").fill("admin-token");
    await page.getByRole("button", { name: "Connect to control plane" }).click();
    await page.getByRole("button", { name: "Network topology" }).click();

    await expect(page.locator(".graph-link-fallback")).toHaveText(expected);
    await expect(page.locator(".managed-link-list")).toContainText(expected);
  });
}

test("management role exposes panel state and a consistent role editor", async ({ page }) => {
  await page.setViewportSize({ width: 2048, height: 1100 });
  await page.addInitScript(() => {
    localStorage.setItem("xirang.language", "en");
    localStorage.setItem("xirang.theme", "dark");
    localStorage.setItem("xirang.refreshSeconds", "60");
  });
  await mockControlPlane(page, "admin");
  await page.route("**/api/v1/nodes", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: [{
    id: "node-management", name: "management-a", status: "ready", os: "linux", architecture: "amd64", distribution: "Ubuntu 24.04", kernelVersion: "6.8.0",
    cpuModel: "AMD EPYC", computeWeight: 76.92, roles: ["management", "compute", "storage"], managementAddress: "192.0.2.10", sshUser: "xirang", group: "lab",
    managementPanelEnabled: true, managementPanelStatus: "ready", capacity: { milliCpu: 96000, memoryBytes: 541165879296, devices: { "nvidia.com/gpu": 3 } },
    allocatable: { milliCpu: 96000, memoryBytes: 541165879296, devices: { "nvidia.com/gpu": 3 } }, allocated: { milliCpu: 0, memoryBytes: 0 },
    revision: 4, createdAt: "2026-08-02T00:00:00Z", updatedAt: "2026-08-02T00:00:01Z", lastSeenAt: "2026-08-02T00:00:01Z",
  }] }) }));
  await page.goto("/");
  await page.getByRole("button", { name: "Token" }).click();
  await page.getByLabel("Bootstrap / API / OIDC Token").fill("admin-token");
  await page.getByRole("button", { name: "Connect to control plane" }).click();
  await page.getByRole("button", { name: "Node status" }).click();
  await expect(page.getByText("Panel enabled")).toBeVisible();
  await page.locator(".action-menu summary").click();
  await page.getByRole("button", { name: "Roles, access and priority" }).click();
  await expect(page.locator(".action-menu")).toHaveJSProperty("open", false);
  await expect(page.getByText("The management role exposes the XIRANG panel on port 8899 of this node", { exact: false })).toBeVisible();
  await expect(page.locator(".app-shell")).toHaveScreenshot("management-node-panel-dark.png", {
    mask: [page.locator(".live-indicator"), page.locator(".sidebar-footer small")],
  });
});

test("administrator can remove an offline node while retaining a tombstone response", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("xirang.language", "en");
    localStorage.setItem("xirang.theme", "dark");
    localStorage.setItem("xirang.refreshSeconds", "60");
  });
  await mockControlPlane(page, "admin");
  const node = {
    id: "node-offline", name: "retired-worker", status: "offline", os: "linux", architecture: "amd64", roles: ["compute"],
    certificateSerial: "expired", certificateRevokedAt: "2026-08-01T00:00:00Z",
    capacity: { milliCpu: 8000, memoryBytes: 16_000_000_000 }, allocatable: { milliCpu: 8000, memoryBytes: 16_000_000_000 },
    allocated: { milliCpu: 0, memoryBytes: 0 }, revision: 4, createdAt: "2026-07-01T00:00:00Z", updatedAt: "2026-08-01T00:00:00Z", lastSeenAt: "2026-08-01T00:00:00Z",
  };
  await page.route("**/api/v1/nodes", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ items: [node] }) }));
  let removalMethod = "";
  await page.route("**/api/v1/nodes/node-offline", (route) => {
    removalMethod = route.request().method();
    return route.fulfill({ contentType: "application/json", body: JSON.stringify({ ...node, status: "removed", removedAt: "2026-08-31T00:00:00Z" }) });
  });

  await page.goto("/");
  await page.getByRole("button", { name: "Token" }).click();
  await page.getByLabel("Bootstrap / API / OIDC Token").fill("admin-token");
  await page.getByRole("button", { name: "Connect to control plane" }).click();
  await page.getByRole("button", { name: "Node status" }).click();
  await page.locator(".action-menu summary").click();
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "Remove node" }).click();
  await expect(page.getByText("retired-worker")).toHaveCount(0);
  expect(removalMethod).toBe("DELETE");
});

async function loginAsAdmin(page: Page) {
  await page.addInitScript(() => { localStorage.setItem("xirang.language", "en"); localStorage.setItem("xirang.theme", "dark"); localStorage.setItem("xirang.refreshSeconds", "60"); });
  await mockControlPlane(page, "admin");
  await page.goto("/");
  await page.getByRole("button", { name: "Token" }).click();
  await page.getByLabel("Bootstrap / API / OIDC Token").fill("admin-token");
  await page.getByRole("button", { name: "Connect to control plane" }).click();
}

// Contrast is checked twice, and the two checks catch different mistakes.
// scripts/audit-theme.mjs reads the token definitions, so it knows --muted is
// legible on --surface. It cannot know which ground a rule actually puts --muted
// on. This walks the rendered pages instead, composites every text element
// against its real ancestry -- through translucent grounds, which is where the
// light theme kept failing -- and holds the result to WCAG AA.
const AUDIT_PAGES = [
  "Cluster overview", "Network topology", "Alert center", "Node status", "Node enrollment",
  "Job scheduling", "Queue policies", "Native builds", "Shared storage", "Usage accounting",
  "Terminal access", "Identity management",
];

for (const theme of ["light", "dark"] as const) {
  test(`every ${theme} page reaches AA contrast as rendered`, async ({ page }) => {
    await page.addInitScript((value) => {
      localStorage.setItem("xirang.language", "en");
      localStorage.setItem("xirang.theme", value);
      localStorage.setItem("xirang.refreshSeconds", "60");
    }, theme);
    await mockControlPlane(page, "admin");
    await page.goto("/");
    await page.getByRole("button", { name: "Token" }).click();
    await page.getByLabel("Bootstrap / API / OIDC Token").fill("admin-token");
    await page.getByRole("button", { name: "Connect to control plane" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Cluster overview" })).toBeVisible();

    const failures: string[] = [];
    for (const label of AUDIT_PAGES) {
      await page.getByRole("button", { name: label, exact: true }).click();
      await page.waitForTimeout(350);
      const found = await page.evaluate(() => {
        // Chrome reports a resolved color-mix() as color(srgb r g b / a) on a
        // 0..1 scale and everything else as rgb()/rgba() on 0..255. Reading the
        // first on the second's scale makes a pale ground look black, which
        // reports contrast failures that are not there.
        const parse = (value: string): number[] | null => {
          if (!value || value === "transparent" || value === "none") return null;
          const srgb = value.match(/color\(\s*srgb\s+([\d.eE+-]+)\s+([\d.eE+-]+)\s+([\d.eE+-]+)(?:\s*\/\s*([\d.eE+-]+))?\s*\)/);
          if (srgb) return [+srgb[1] * 255, +srgb[2] * 255, +srgb[3] * 255, srgb[4] === undefined ? 1 : +srgb[4]];
          const parts = value.match(/[\d.eE+-]+/g);
          return parts ? parts.slice(0, 3).map(Number).concat([parts[3] === undefined ? 1 : +parts[3]]) : null;
        };
        const channel = (v: number) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
        const luminance = (c: number[]) => 0.2126 * channel(c[0]) + 0.7152 * channel(c[1]) + 0.0722 * channel(c[2]);
        const over = (fg: number[], bg: number[]) => fg.slice(0, 3).map((c, i) => c * fg[3] + bg[i] * (1 - fg[3])).concat([1]);
        const groundOf = (element: Element) => {
          let accumulated: number[] | null = null;
          for (let node: Element | null = element; node; node = node.parentElement) {
            const background = parse(getComputedStyle(node).backgroundColor);
            if (background && background[3] > 0) {
              accumulated = accumulated ? over(accumulated, background) : background;
              if (accumulated[3] >= 0.999) return accumulated;
            }
          }
          return accumulated && accumulated[3] >= 0.999 ? accumulated : [255, 255, 255, 1];
        };
        const results: string[] = [];
        for (const element of Array.from(document.querySelectorAll("body *"))) {
          const carriesText = Array.from(element.childNodes).some((n) => n.nodeType === 3 && (n.textContent ?? "").trim().length > 1);
          if (!carriesText) continue;
          const box = element.getBoundingClientRect();
          if (box.width < 2 || box.height < 2) continue;
          const style = getComputedStyle(element);
          if (style.visibility === "hidden" || style.display === "none" || +style.opacity < 0.15) continue;
          const foreground = parse(style.color);
          if (!foreground) continue;
          const ground = groundOf(element);
          const solid = foreground[3] >= 0.999 ? foreground : over(foreground, ground);
          const [high, low] = [luminance(solid), luminance(ground)].sort((a, b) => b - a);
          const ratio = (high + 0.05) / (low + 0.05);
          const size = parseFloat(style.fontSize);
          const large = size >= 24 || (size >= 18.66 && +style.fontWeight >= 700);
          const needed = large ? 3 : 4.5;
          if (ratio < needed) {
            results.push(`${ratio.toFixed(2)}:1 (needs ${needed}, ${size}px) <${element.tagName.toLowerCase()} class="${element.className}"> "${(element.textContent ?? "").trim().slice(0, 40)}"`);
          }
        }
        return Array.from(new Set(results));
      });
      for (const entry of found) failures.push(`${label}: ${entry}`);
    }
    expect(failures, `unreadable text in the ${theme} theme`).toEqual([]);
  });
}

async function mockControlPlane(page: Page, role: Role) {
  let groups = [
    { id: "group_unlimited", name: "unlimited-lab", resourceQuota: { milliCpu: 0, memoryBytes: 0, ephemeralStorageBytes: 0 }, storageQuotaBytes: 0, maxConcurrentJobs: 0, maxJobTimeoutSeconds: 604800, disabled: false, createdAt: "2026-08-02T00:00:00Z", updatedAt: "2026-08-02T00:00:00Z" },
    { id: "group_userlabs", name: "UserLabs", resourceQuota: { milliCpu: 8000, memoryBytes: 16_000_000_000, ephemeralStorageBytes: 0 }, storageQuotaBytes: 100_000_000_000, maxConcurrentJobs: 4, maxJobTimeoutSeconds: 604800, disabled: false, createdAt: "2026-08-02T00:00:00Z", updatedAt: "2026-08-02T00:00:00Z" },
  ];
  let accessSettings = { defaultUserGroupId: "", updatedAt: "2026-08-02T00:00:00Z" };
  let localUsers = [{ id: "user_admin", username: "admin", displayName: "E2E Administrator", role: "admin", groupIds: ["group_userlabs"], status: "active", createdAt: "2026-08-02T00:00:00Z", approvedAt: "2026-08-02T00:00:00Z" }];
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    switch (url.pathname) {
      case "/api/v1/version":
        return json({ name: "XIRANG", version: "e2e", protocolVersion: "v1", commit: "test", buildTime: "2026-08-02T00:00:00Z", goVersion: "go1.25", os: "linux", architecture: "amd64" });
      case "/api/v1/setup/status":
        return json({ setupRequired: false });
      case "/api/v1/access/me":
        return json({ subject: `${role}-e2e`, displayName: role === "admin" ? "E2E Administrator" : "E2E User", role, permissions: role === "admin" ? ["*"] : ["cluster.read", "jobs.manage"], authMethod: "api_token" });
      case "/api/v1/access/users":
        return json({ items: localUsers });
      case "/api/v1/access/groups":
        return json({ items: groups });
      case "/api/v1/access/settings":
        if (request.method() === "PUT") {
          const update = request.postDataJSON() as { defaultUserGroupId?: string };
          accessSettings = { defaultUserGroupId: update.defaultUserGroupId ?? "", updatedAt: "2026-08-02T00:00:01Z" };
        }
        return json(accessSettings);
      case "/api/v1/system/telemetry-interval":
        return json({ intervalSeconds: 60, minimumSeconds: 2, maximumSeconds: 60 });
      case "/api/v1/nodes":
        return json({ items: [] });
      case "/api/v1/jobs":
      case "/api/v1/builds":
        return json({ items: [] });
      case "/api/v1/build-retention":
        return json({ maximumRecords: 500, maximumAgeSeconds: 2592000 });
      case "/api/v1/job-templates":
        return json({ items: [{ id: "template_arm", name: "ARM simulation", description: "ARM64 test template", script: "XIRANG 1\nproject = default\nqueue = default\npriority = 0\nattempts = 1\nartifact = file:///shared/app\nsha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\narchitecture = arm64\nentrypoint = app\ncpu = 4\nmemory = 8G\n", revision: 1, createdBy: "admin", updatedBy: "admin", createdAt: "2026-08-02T00:00:00Z", updatedAt: "2026-08-02T00:00:00Z" }] });
      case "/api/v1/build-workers":
        return json({ items: [{ id: "agent:node-arm", architecture: "arm64", toolchains: ["go", "cc"], toolchainCapabilities: [{ name: "go", version: "1.25", targetArchitectures: ["amd64", "arm64"], executables: ["go"] }, { name: "cc", version: "gcc", targetArchitectures: ["arm64"], executables: ["cc"] }], isolation: "cgroup-v2", ready: true, supportsCancellation: true }, { id: "local-amd64", architecture: "amd64", toolchains: ["go"], toolchainCapabilities: [{ name: "go", version: "1.25", targetArchitectures: ["amd64", "arm64"], executables: ["go"] }], isolation: "cgroup-v2", ready: true, supportsCancellation: true }] });
      case "/api/v1/shared-spaces":
        return json({ items: [{ id: "share_test", name: "Research", sourceNodeId: "storage", sourcePath: "/srv/share", mountPath: "/shared", target: "all", status: "active", revision: 1, createdAt: "2026-08-02T00:00:00Z", updatedAt: "2026-08-02T00:00:00Z", usedBytes: 4, availableBytes: 1000, totalBytes: 1004, capacityObservedAt: "2026-08-02T00:00:00Z" }] });
      case "/api/v1/shared-spaces/share_test/files":
        return json({ items: [{ path: "input.dat", name: "input.dat", directory: false, sizeBytes: 4, modifiedAt: "2026-08-02T00:00:00Z", owner: "E2E User", ownedByUser: true }] });
      default:
        if (url.pathname.startsWith("/api/v1/access/users/") && request.method() === "PATCH") {
          const id = url.pathname.split("/").at(-1);
          const update = request.postDataJSON() as { status?: string; role?: string; groupIds?: string[] };
          localUsers = localUsers.map((user) => user.id === id ? { ...user, ...update } : user);
          return json(localUsers.find((user) => user.id === id));
        }
        if (url.pathname.startsWith("/api/v1/access/groups/") && request.method() === "DELETE") {
          const id = url.pathname.split("/").at(-1);
          const detached = localUsers.filter((user) => user.groupIds.includes(id ?? "")).length;
          localUsers = localUsers.map((user) => ({ ...user, groupIds: user.groupIds.filter((groupID) => groupID !== id) }));
          groups = groups.filter((group) => group.id !== id);
          if (accessSettings.defaultUserGroupId === id) accessSettings = { ...accessSettings, defaultUserGroupId: "" };
          return route.fulfill({ status: 204, headers: { "X-Xirang-Detached-Users": String(detached) }, body: "" });
        }
        if (url.pathname.startsWith("/api/v1/access/groups/") && request.method() === "PUT") {
          const id = url.pathname.split("/").at(-1);
          const update = request.postDataJSON();
          groups = groups.map((group) => group.id === id ? { ...group, ...update } : group);
          return json(groups.find((group) => group.id === id));
        }
        if (url.pathname.includes("/observations")) return json({ items: [] });
        return json({ items: [] });
    }
  });
}
