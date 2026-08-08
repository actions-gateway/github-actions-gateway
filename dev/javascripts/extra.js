// Progressive, accessibility-first scroll reveals for the marketing pages.
// Content is fully visible without JS; the hidden initial state is only applied
// once we know IntersectionObserver is available and the user allows motion.
(function () {
  if (!("IntersectionObserver" in window)) return;
  if (window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

  // Only the landing and "why GAG" pages — keep the dense reference docs calm.
  var isMarketing = !!document.querySelector(".gag-hero") || /why-gag/.test(location.pathname);
  if (!isMarketing) return;

  document.documentElement.classList.add("gag-reveal-ready");

  var targets = document.querySelectorAll(
    ".md-content h2, .gag-pillars .grid.cards > ul > li, .gag-flow, .gag-stat, .md-typeset table:not([class])"
  );
  targets.forEach(function (el) {
    el.classList.add("gag-reveal");
  });

  // Stagger the benefit tiles within each row of three.
  document.querySelectorAll(".gag-pillars .grid.cards > ul > li").forEach(function (el, i) {
    el.style.transitionDelay = (i % 3) * 70 + "ms";
  });

  // Stagger the why-GAG stat band so its four numbers ripple in left-to-right.
  document.querySelectorAll(".gag-stat").forEach(function (el, i) {
    el.style.transitionDelay = i * 60 + "ms";
  });

  var io = new IntersectionObserver(
    function (entries) {
      entries.forEach(function (entry) {
        if (entry.isIntersecting) {
          entry.target.classList.add("is-visible");
          io.unobserve(entry.target);
        }
      });
    },
    { rootMargin: "0px 0px -8% 0px", threshold: 0.06 }
  );

  targets.forEach(function (el) {
    io.observe(el);
  });
})();

// Chip-style filters: operations personas (a "Personas" table) and the design
// "Reading Paths by Role". Progressive enhancement — without JS the underlying
// table/paragraphs render normally (also how they appear on github.com).
(function () {
  // Selection lives in the query string, so a filtered view is a link the
  // reader can copy. replaceState rather than pushState: filtering is not
  // navigation, and a chip row would otherwise fill the back button with
  // states nobody wants to walk back through.
  function writeParam(name, value) {
    if (!window.history || !history.replaceState) return;
    var url = new URL(location.href);
    if (value && value !== "All") url.searchParams.set(name, value);
    else url.searchParams.delete(name);
    history.replaceState(null, "", url);
  }

  // Bars sharing a query key move together. STATUS renders one bar per
  // dimension per table, so a reload applies `?label=ci` to all of them; a
  // click has to do the same or the shared link and the live page disagree.
  var barsByParam = {};

  function applySelection(bar, label) {
    var chips = Array.prototype.slice.call(bar.querySelectorAll(".persona-chip"));
    var chosen = null;
    chips.forEach(function (c) {
      if (c.dataset.persona === label) chosen = c;
    });
    // A bar with no chip for this value falls back to its default rather than
    // filtering away every row for a label its own table never uses.
    chosen = chosen || chips[0];
    chips.forEach(function (c) {
      c.setAttribute("aria-pressed", String(c === chosen));
    });
    bar.gagSelect(chosen.dataset.persona);
  }

  // A row of single-select chips; the first label is the default ("All").
  // `param` names the query key this bar owns, and is what makes the selection
  // both shareable and restorable.
  function chipBar(labels, ariaLabel, onSelect, param) {
    var bar = document.createElement("div");
    bar.className = "persona-bar";
    bar.setAttribute("role", "group");
    bar.setAttribute("aria-label", ariaLabel);
    bar.gagSelect = onSelect;
    labels.forEach(function (label, i) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "persona-chip";
      b.dataset.persona = label;
      b.textContent = label;
      b.setAttribute("aria-pressed", String(i === 0));
      b.addEventListener("click", function () {
        if (!param) return applySelection(bar, label);
        writeParam(param, label);
        barsByParam[param].forEach(function (other) {
          applySelection(other, label);
        });
      });
      bar.appendChild(b);
    });

    // Restore an incoming selection, so a shared link opens already filtered.
    if (param) {
      barsByParam[param] = barsByParam[param] || [];
      barsByParam[param].push(bar);
      var want = new URLSearchParams(location.search).get(param);
      if (want) applySelection(bar, want);
    }
    return bar;
  }

  // Operations index: a "Personas" table -> chips on top, pills per row.
  document.querySelectorAll(".md-typeset table:not([class])").forEach(function (table) {
    var headers = Array.prototype.map.call(table.querySelectorAll("thead th"), function (th) {
      return th.textContent.trim().toLowerCase();
    });
    var col = headers.indexOf("personas");
    if (col < 0) return;

    var rows = Array.prototype.slice.call(table.querySelectorAll("tbody tr"));
    var personas = [];
    rows.forEach(function (row) {
      var cell = row.cells[col];
      var tags = cell.textContent.split(",").map(function (s) { return s.trim(); }).filter(Boolean);
      row.setAttribute("data-personas", tags.join("|"));
      cell.innerHTML = tags.map(function (t) {
        return '<button type="button" class="persona-pill" data-persona="' +
          t.replace(/"/g, "&quot;") + '">' + t + "</button>";
      }).join(" ");
      tags.forEach(function (t) {
        if (t !== "All" && personas.indexOf(t) < 0) personas.push(t);
      });
    });
    personas.sort();

    var bar = chipBar(["All"].concat(personas), "Filter by persona", function (label) {
      rows.forEach(function (row) {
        var tags = row.getAttribute("data-personas").split("|");
        var show = label === "All" || tags.indexOf(label) >= 0 || tags.indexOf("All") >= 0;
        row.style.display = show ? "" : "none";
      });
    }, "persona");
    var anchor = table.closest(".md-typeset__scrollwrap") || table;
    anchor.parentNode.insertBefore(bar, anchor);

    // Clicking a pill in a row selects the matching chip (and filters).
    table.addEventListener("click", function (e) {
      var pill = e.target.closest(".persona-pill");
      if (!pill || !pill.dataset.persona) return;
      var chip = bar.querySelector('.persona-chip[data-persona="' +
        (window.CSS && CSS.escape ? CSS.escape(pill.dataset.persona) : pill.dataset.persona) + '"]');
      if (chip) chip.click();
    });
  });

  // Design index: "Reading Paths by Role" -> chips that show one role's path.
  var h = document.getElementById("reading-paths-by-role");
  if (h) {
    var paths = [];
    var el = h.nextElementSibling;
    while (el && el.tagName !== "HR" && !/^H[1-3]$/.test(el.tagName)) {
      if (el.tagName === "P" && el.querySelector("strong")) paths.push(el);
      el = el.nextElementSibling;
    }
    if (paths.length) {
      var roles = paths.map(function (p) {
        var role = p.querySelector("strong").textContent.trim();
        p.setAttribute("data-role", role);
        return role;
      });
      var rbar = chipBar(["All"].concat(roles), "Filter reading paths by role", function (label) {
        paths.forEach(function (p) {
          p.style.display = (label === "All" || p.getAttribute("data-role") === label) ? "" : "none";
        });
      }, "role");
      h.parentNode.insertBefore(rbar, h.nextElementSibling);
    }
  }

  // Backlog tables (STATUS.md's Queue, Deferred, Flake watch and Progress) ->
  // one chip bar per dimension: label, status, size. The three intersect.
  // Recognised by shape rather than by page, but narrowly: a Labels column
  // alone would also match the metric tables in the design and observability
  // docs, so an ID/Item first column is required too.
  var STATUS_NAMES = [["🔲", "Ready"], ["🚫", "Blocked"], ["✅", "Done"], ["⚠️", "Open"]];
  var SIZE_ORDER = ["S", "M", "L"];

  // Emoji in the source carry a variation selector; comparisons drop it.
  function plain(s) {
    return s.replace(/️/g, "").trim();
  }

  function byOrder(order) {
    return function (a, b) {
      var ia = order.indexOf(a), ib = order.indexOf(b);
      if (ia < 0 && ib < 0) return a.localeCompare(b);
      if (ia < 0) return 1;
      if (ib < 0) return -1;
      return ia - ib;
    };
  }

  document.querySelectorAll(".md-typeset table:not([class])").forEach(function (table) {
    var headers = Array.prototype.map.call(table.querySelectorAll("thead th"), function (th) {
      return th.textContent.trim().toLowerCase();
    });
    if (headers.indexOf("labels") < 0) return;
    if (headers[0] !== "id" && headers[0] !== "item") return;

    function column() {
      for (var i = 0; i < arguments.length; i++) {
        var at = headers.indexOf(arguments[i]);
        if (at >= 0) return at;
      }
      return -1;
    }
    var cols = { label: headers.indexOf("labels"), status: column("st", "status"), size: column("sz", "size") };

    var rows = Array.prototype.slice.call(table.querySelectorAll("tbody tr"));
    var seen = { label: [], status: [], size: [] };
    rows.forEach(function (row) {
      // Labels are backticked in the markdown, so each renders as its own <code>.
      var labels = Array.prototype.map.call(row.cells[cols.label].querySelectorAll("code"), function (c) {
        c.classList.add("backlog-label");
        return c.textContent.trim();
      });
      var status = "";
      if (cols.status >= 0) {
        var cell = plain(row.cells[cols.status].textContent);
        STATUS_NAMES.forEach(function (pair) {
          if (cell.indexOf(plain(pair[0])) >= 0) status = pair[1];
        });
      }
      var size = cols.size >= 0 ? row.cells[cols.size].textContent.trim() : "";

      row.setAttribute("data-labels", labels.join("|"));
      row.setAttribute("data-status", status);
      row.setAttribute("data-size", size);
      labels.forEach(function (l) {
        if (seen.label.indexOf(l) < 0) seen.label.push(l);
      });
      [["status", status], ["size", size]].forEach(function (pair) {
        if (pair[1] && seen[pair[0]].indexOf(pair[1]) < 0) seen[pair[0]].push(pair[1]);
      });
    });
    if (!seen.label.length) return;

    seen.label.sort();
    seen.status.sort(byOrder(STATUS_NAMES.map(function (p) { return p[1]; })));
    seen.size.sort(byOrder(SIZE_ORDER));

    var picked = { label: "All", status: "All", size: "All" };
    var count = document.createElement("p");
    count.className = "backlog-count";
    count.setAttribute("role", "status");
    count.setAttribute("aria-live", "polite");

    function matches(row, key, value) {
      if (value === "All") return true;
      if (key === "label") return row.getAttribute("data-labels").split("|").indexOf(value) >= 0;
      return row.getAttribute("data-" + key) === value;
    }

    // A dimension every row shares filters nothing — the Progress table's
    // all-✅ Status column would otherwise render a chip bar that can only
    // ever show every row.
    function discriminates(key) {
      if (seen[key].length > 1) return true;
      if (!seen[key].length) return false;
      return rows.some(function (row) { return !matches(row, key, seen[key][0]); });
    }

    function apply() {
      var shown = 0;
      rows.forEach(function (row) {
        var show = matches(row, "label", picked.label) &&
          matches(row, "status", picked.status) &&
          matches(row, "size", picked.size);
        row.style.display = show ? "" : "none";
        if (show) shown++;
      });
      count.textContent = shown === rows.length
        ? rows.length + " items"
        : shown + " of " + rows.length + " items";
    }

    var filters = document.createElement("div");
    filters.className = "backlog-filters";
    var bars = {};
    [["label", "Label"], ["status", "Status"], ["size", "Size"]].forEach(function (pair) {
      var key = pair[0];
      if (!discriminates(key)) return;
      var row = document.createElement("div");
      row.className = "backlog-filter";
      var legend = document.createElement("span");
      legend.className = "backlog-filter__legend";
      legend.textContent = pair[1];
      // Each dimension owns its own query key, so the three intersect in the
      // URL exactly as they do in the table.
      bars[key] = chipBar(["All"].concat(seen[key]), "Filter by " + pair[1].toLowerCase(), function (label) {
        picked[key] = label;
        apply();
      }, key);
      row.appendChild(legend);
      row.appendChild(bars[key]);
      filters.appendChild(row);
    });
    if (!bars.label && !bars.status && !bars.size) return;
    filters.appendChild(count);

    var anchor = table.closest(".md-typeset__scrollwrap") || table;
    anchor.parentNode.insertBefore(filters, anchor);
    apply();

    // Clicking a label in a row selects its chip, as the persona pills do.
    table.addEventListener("click", function (e) {
      var code = e.target.closest("code.backlog-label");
      if (!code || !bars.label) return;
      var want = code.textContent.trim();
      bars.label.querySelectorAll(".persona-chip").forEach(function (c) {
        if (c.dataset.persona === want) c.click();
      });
    });
  });
})();

// Savings calculator (Appendix F): enhance an empty `.gag-calc` mount into an
// interactive estimator of monthly savings vs ARC. Without JS the mount is empty
// and the hand-worked example in the markdown is the static fallback — same
// progressive-enhancement contract as the persona chips above.
//
// Model (honest, deliberately conservative): active job time costs the same in
// BOTH systems (one pod per job, for the job's duration), so it cancels out. The
// only difference is ARC's idle `minRunners` floor, billed 24/7. The estimated
// saving is exactly that eliminated floor. See appendix-f-cost-model.md § F.5.
(function () {
  var mount = document.querySelector(".gag-calc");
  if (!mount) return;

  var HOURS_PER_MONTH = 730; // 365 × 24 ÷ 12
  var DAYS_PER_MONTH = HOURS_PER_MONTH / 24; // ≈ 30.42

  function attrNum(attr, dflt) {
    var v = parseFloat(mount.getAttribute(attr));
    return isFinite(v) ? v : dflt;
  }

  var state = {
    jobs: attrNum("data-jobs", 200),
    duration: attrNum("data-duration", 12),
    idle: attrNum("data-idle", 10),
    rate: attrNum("data-rate", 4.10)
  };

  // Instance presets — list prices cited in appendix-f § F.0. AMD Instinct rates
  // are representative on-demand neocloud figures and are more volatile than the
  // AWS NVIDIA rates; the caveat in § F.0 spells that out.
  var presets = [
    { label: "A100 GPU", sub: "p4d.24xlarge ⅛", rate: 4.10 },
    { label: "A10G GPU", sub: "g5.xlarge", rate: 1.01 },
    { label: "T4 GPU", sub: "g4dn.xlarge", rate: 0.53 },
    { label: "MI300X GPU", sub: "AMD · on-demand", rate: 2.00 },
    { label: "MI325X GPU", sub: "AMD · on-demand", rate: 2.10 },
    { label: "MI355X GPU", sub: "AMD · scarce", rate: 3.00 },
    { label: "CPU node", sub: "m6i.4xlarge", rate: 0.77 }
  ];

  function dollars(n) {
    return "$" + Math.max(0, Math.round(n)).toLocaleString("en-US");
  }
  function rateStr(n) {
    return "$" + n.toFixed(2);
  }

  var fields = [
    { key: "jobs", label: "Jobs per day", step: 10 },
    { key: "duration", label: "Avg job duration (min)", step: 1 },
    { key: "idle", label: "Idle runners ARC holds (minRunners × sets)", step: 1 },
    { key: "rate", label: "Cost per runner-hour ($)", step: 0.01 }
  ];

  var form = document.createElement("form");
  form.className = "gag-calc__form";
  form.setAttribute("aria-label", "Estimate monthly savings versus ARC");
  form.addEventListener("submit", function (e) { e.preventDefault(); });

  var inputs = {};
  fields.forEach(function (f) {
    var wrap = document.createElement("label");
    wrap.className = "gag-calc__field";
    var span = document.createElement("span");
    span.className = "gag-calc__field-label";
    span.textContent = f.label;
    var input = document.createElement("input");
    input.type = "number";
    input.inputMode = "decimal";
    input.min = "0";
    input.step = String(f.step);
    input.value = String(state[f.key]);
    input.addEventListener("input", function () {
      var v = parseFloat(input.value);
      state[f.key] = isFinite(v) && v >= 0 ? v : 0;
      render();
    });
    inputs[f.key] = input;
    wrap.appendChild(span);
    wrap.appendChild(input);
    form.appendChild(wrap);
  });

  var presetBar = document.createElement("div");
  presetBar.className = "gag-calc__presets";
  presetBar.setAttribute("role", "group");
  presetBar.setAttribute("aria-label", "Per-runner cost presets");
  presets.forEach(function (p) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "gag-calc__preset";
    b.innerHTML = "<strong>" + p.label + "</strong><small>" + p.sub +
      " · " + rateStr(p.rate) + "/hr</small>";
    b.addEventListener("click", function () {
      state.rate = p.rate;
      inputs.rate.value = String(p.rate);
      render();
    });
    presetBar.appendChild(b);
  });

  var out = document.createElement("div");
  out.className = "gag-calc__out";
  out.setAttribute("role", "status");
  out.setAttribute("aria-live", "polite");

  mount.appendChild(form);
  mount.appendChild(presetBar);
  mount.appendChild(out);

  function cell(label, value, sub, win) {
    return '<div class="gag-calc__cell' + (win ? " gag-calc__cell--win" : "") + '">' +
      '<span class="gag-calc__cell-num">' + value + "</span>" +
      '<span class="gag-calc__cell-label">' + label + "</span>" +
      '<span class="gag-calc__cell-sub">' + sub + "</span></div>";
  }

  function render() {
    var activeHours = state.jobs * (state.duration / 60) * DAYS_PER_MONTH;
    var activeCost = activeHours * state.rate; // paid by BOTH systems
    var idleCost = state.idle * state.rate * HOURS_PER_MONTH; // ARC only
    var arcTotal = activeCost + idleCost;
    var saving = idleCost;
    var pct = arcTotal > 0 ? Math.round((saving / arcTotal) * 100) : 0;

    out.innerHTML =
      '<div class="gag-calc__grid">' +
        cell("ARC / month", dollars(arcTotal), "active jobs + idle floor") +
        cell("This system / month", dollars(activeCost), "active jobs only") +
        cell("You save / month", dollars(saving), pct + "% of ARC's bill", true) +
        cell("You save / year", dollars(saving * 12), "at this workload", true) +
      "</div>" +
      '<p class="gag-calc__note">Saving = the idle-runner floor ARC holds 24/7 (' +
      Math.round(state.idle).toLocaleString("en-US") + " × " + rateStr(state.rate) +
      "/hr × 730 hr). Active job time (~" +
      Math.round(activeHours).toLocaleString("en-US") +
      " hr/mo) costs the same in both systems, so it cancels out. " +
      "Estimate from list prices — verify your own contracted rates.</p>";
  }

  render();
})();

// Per-doc audience: upgrade a leading "> **Audience:** ..." blockquote into pills.
// On github.com (and without JS) it stays a readable blockquote.
(function () {
  document.querySelectorAll(".md-content blockquote").forEach(function (bq) {
    var m = bq.textContent.trim().match(/^Audience:\s*(.+)$/i);
    if (!m) return;
    var tags = m[1].split(",").map(function (s) { return s.trim(); }).filter(Boolean);
    var div = document.createElement("div");
    div.className = "persona-pills-top";
    div.setAttribute("aria-label", "Audience");
    // Link back to the operations index, pre-filtered to this persona.
    div.innerHTML = tags.map(function (t) {
      return '<a class="persona-pill" href="../?persona=' + encodeURIComponent(t) +
        '" title="See all ' + t + ' docs">' + t + "</a>";
    }).join(" ");
    bq.parentNode.replaceChild(div, bq);
  });
})();
