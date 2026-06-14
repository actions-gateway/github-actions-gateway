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
    ".md-content h2, .gag-pillars .grid.cards > ul > li, .gag-flow, .md-typeset table:not([class])"
  );
  targets.forEach(function (el) {
    el.classList.add("gag-reveal");
  });

  // Stagger the benefit tiles within each row of three.
  document.querySelectorAll(".gag-pillars .grid.cards > ul > li").forEach(function (el, i) {
    el.style.transitionDelay = (i % 3) * 70 + "ms";
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
        return '<span class="persona-pill">' + t + "</span>";
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
