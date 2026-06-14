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
