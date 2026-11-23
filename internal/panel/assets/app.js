// Panel behavior that HTML alone cannot express. It never decides access:
// every request it makes is authorized by the server like any other.
(function () {
  "use strict";

  var reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  function onSessionExpired() {
    window.location.assign("/login");
  }

  // The live stream announces a refresh of named scopes. Each region declares
  // which scope it follows and the fragment that redraws it. A region the
  // reader is working in is not replaced; a notice offers the new data.
  function refresh(scopes) {
    var regions = document.querySelectorAll("[data-live-scope]");
    regions.forEach(function (region) {
      if (scopes.indexOf(region.getAttribute("data-live-scope")) === -1) {
        return;
      }
      if (region.getAttribute("data-live-hold") === "true" && region.contains(document.activeElement) && document.activeElement !== document.body) {
        var notice = document.getElementById(region.getAttribute("data-live-notice"));
        if (notice) {
          notice.hidden = false;
        }
        return;
      }
      htmx.trigger(region, "live-refresh");
    });
  }

  // The SSE extension owns the connection and its reconnection backoff; this
  // only routes the closed scopes it announces to the regions that follow them.
  function setStatus(text) {
    var status = document.getElementById("live-status");
    if (status) {
      status.textContent = text;
    }
  }

  document.addEventListener("sse:refresh", function (event) {
    var data = event.detail && typeof event.detail.data === "string" ? event.detail.data : "";
    refresh(data.split(","));
    setStatus("Live");
  });
  document.addEventListener("htmx:sseOpen", function () {
    setStatus("Live");
  });
  document.addEventListener("htmx:sseError", function () {
    setStatus("Reconnecting");
  });
  document.addEventListener("htmx:sseClose", function (event) {
    if (event.detail && event.detail.type === "message") {
      onSessionExpired();
    }
  });

  // Charts are drawn from the table next to them, which stays the accessible
  // source of the same numbers.
  function drawCharts(scope) {
    if (!window.uPlot) {
      return;
    }
    (scope || document).querySelectorAll("[data-chart-table]").forEach(function (holder) {
      var table = document.getElementById(holder.getAttribute("data-chart-table"));
      if (!table) {
        return;
      }
      var xs = [];
      var attempts = [];
      var failures = [];
      table.querySelectorAll("tbody tr").forEach(function (row) {
        var cells = row.querySelectorAll("td");
        xs.push(Number(row.getAttribute("data-x")));
        attempts.push(row.getAttribute("data-present") === "true" ? Number(cells[1].textContent) : null);
        failures.push(row.getAttribute("data-present") === "true" ? Number(cells[2].textContent) : null);
      });
      holder.textContent = "";
      var width = Math.max(240, holder.clientWidth || 320);
      var chart = new uPlot({
        width: width,
        height: 140,
        legend: { show: true },
        cursor: { drag: { x: false, y: false } },
        series: [
          {},
          { label: "Attempts", stroke: "#2563eb", spanGaps: false },
          { label: "Failures", stroke: "#dc2626", spanGaps: false }
        ],
        axes: [{}, {}]
      }, [xs, attempts, failures], holder);
      holder.chart = chart;
    });
  }

  window.addEventListener("resize", function () {
    document.querySelectorAll("[data-chart-table]").forEach(function (holder) {
      if (holder.chart) {
        holder.chart.setSize({ width: Math.max(240, holder.clientWidth || 320), height: 140 });
      }
    });
  });

  document.addEventListener("htmx:afterSwap", function (event) {
    drawCharts(event.target);
    var region = event.target.closest ? event.target.closest("[data-live-scope]") : null;
    if (region) {
      var notice = document.getElementById(region.getAttribute("data-live-notice"));
      if (notice) {
        notice.hidden = true;
      }
    }
  });

  document.addEventListener("htmx:beforeSwap", function (event) {
    if (event.detail.xhr && event.detail.xhr.status === 401) {
      event.detail.shouldSwap = false;
      onSessionExpired();
    }
  });

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest("[data-live-apply]") : null;
    if (!button) {
      return;
    }
    var region = document.getElementById(button.getAttribute("data-live-apply"));
    if (region) {
      htmx.trigger(region, "live-refresh");
    }
  });

  if (reduceMotion) {
    document.documentElement.classList.add("reduce-motion");
  }
  document.addEventListener("DOMContentLoaded", function () {
    drawCharts(document);
  });
})();
