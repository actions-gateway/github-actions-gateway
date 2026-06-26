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
  // A row of single-select chips; the first label is the default ("All").
  function chipBar(labels, ariaLabel, onSelect) {
    var bar = document.createElement("div");
    bar.className = "persona-bar";
    bar.setAttribute("role", "group");
    bar.setAttribute("aria-label", ariaLabel);
    labels.forEach(function (label, i) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "persona-chip";
      b.dataset.persona = label;
      b.textContent = label;
      b.setAttribute("aria-pressed", String(i === 0));
      b.addEventListener("click", function () {
        bar.querySelectorAll(".persona-chip").forEach(function (c) {
          c.setAttribute("aria-pressed", String(c === b));
        });
        onSelect(label);
      });
      bar.appendChild(b);
    });
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
    });
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

    // Honor ?persona=... (e.g. arriving from a doc's audience pill).
    var want = new URLSearchParams(location.search).get("persona");
    if (want) {
      bar.querySelectorAll(".persona-chip").forEach(function (c) {
        if (c.dataset.persona === want) c.click();
      });
    }
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
      });
      h.parentNode.insertBefore(rbar, h.nextElementSibling);
    }
  }
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
