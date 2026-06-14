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

// Persona filter (operations index): turn the "Personas" table into a filterable
// list — a row of persona chips on top, pills per row. Progressive enhancement:
// without JS it's a plain table (also how it renders on github.com).
(function () {
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

    var bar = document.createElement("div");
    bar.className = "persona-bar";
    bar.setAttribute("role", "group");
    bar.setAttribute("aria-label", "Filter by persona");

    function select(label) {
      bar.querySelectorAll(".persona-chip").forEach(function (c) {
        c.setAttribute("aria-pressed", String(c.dataset.persona === label));
      });
      rows.forEach(function (row) {
        var tags = row.getAttribute("data-personas").split("|");
        var show = label === "All" || tags.indexOf(label) >= 0 || tags.indexOf("All") >= 0;
        row.style.display = show ? "" : "none";
      });
    }

    ["All"].concat(personas).forEach(function (label) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "persona-chip";
      b.dataset.persona = label;
      b.textContent = label;
      b.setAttribute("aria-pressed", String(label === "All"));
      b.addEventListener("click", function () { select(label); });
      bar.appendChild(b);
    });

    var anchor = table.closest(".md-typeset__scrollwrap") || table;
    anchor.parentNode.insertBefore(bar, anchor);
  });
})();
