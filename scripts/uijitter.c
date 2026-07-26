/*
 * uijitter.c — measure scheduling latency at the QoS tier the desktop runs at.
 *
 * The macOS failure mode scripts/local-throttle.sh exists to prevent is the
 * WindowServer watchdog firing when a build starves the compositor. That is a
 * cliff: by the time it trips, the GUI is already gone. This probe measures the
 * approach to the cliff instead — it runs a thread at QOS_CLASS_USER_INTERACTIVE
 * (where the compositor lives), wakes it on a fixed frame-rate period, and
 * records how late each wake actually is.
 *
 * Overshoot is the number that matters: it is the delay a UI thread would have
 * experienced had it wanted to render that frame. A build that keeps p99
 * overshoot near zero is invisible to the desktop no matter how much CPU it
 * burns; one that pushes p99 into the hundreds of milliseconds is producing
 * visible stutter well before any watchdog fires.
 *
 * Runs until SIGINT/SIGTERM, then prints a percentile report to stdout, so a
 * harness can start it, run a workload, and signal it to collect the result.
 * An optional duration makes it self-terminating for a standalone spot check,
 * so no external sleep/timeout wrapper is needed.
 *
 * Build:  clang -O2 -o tmp/uijitter tmp/uijitter.c
 * Usage:  tmp/uijitter [period_ms] [duration_s]
 *         period_ms   default 16.667 — a 60 Hz frame
 *         duration_s  default 0 — run until signaled
 */
#include <errno.h>
#include <pthread/qos.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

#define DEFAULT_PERIOD_MS 16.667
#define INITIAL_CAPACITY 65536
/* Overshoot past which a dropped frame would be perceptible, and past which the
 * stall is severe enough to read as a hang. Reported as counts, not just
 * percentiles, because a handful of multi-second stalls matters even when p99
 * looks fine. */
#define VISIBLE_STUTTER_MS 50.0
#define SEVERE_STALL_MS 250.0

static volatile sig_atomic_t stop_flag = 0;

static void on_signal(int signo) {
	(void)signo;
	stop_flag = 1;
}

static int cmp_double(const void *a, const void *b) {
	double x = *(const double *)a, y = *(const double *)b;
	return (x > y) - (x < y);
}

/* now_ms returns a monotonic timestamp in milliseconds. */
static double now_ms(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (double)ts.tv_sec * 1000.0 + (double)ts.tv_nsec / 1e6;
}

/* sleep_ms suspends for the given duration, resuming across signal interrupts
 * only when they are not our stop signal. */
static void sleep_ms(double ms) {
	if (ms <= 0) return;
	struct timespec req;
	req.tv_sec = (time_t)(ms / 1000.0);
	req.tv_nsec = (long)((ms - (double)req.tv_sec * 1000.0) * 1e6);
	while (nanosleep(&req, &req) == -1 && errno == EINTR && !stop_flag) {
		/* resume the remaining interval */
	}
}

/* percentile returns the p-th percentile of a sorted array. */
static double percentile(const double *sorted, size_t n, double p) {
	if (n == 0) return 0.0;
	size_t idx = (size_t)(p / 100.0 * (double)(n - 1));
	return sorted[idx];
}

int main(int argc, char **argv) {
	double period_ms = (argc > 1) ? atof(argv[1]) : DEFAULT_PERIOD_MS;
	if (period_ms <= 0) period_ms = DEFAULT_PERIOD_MS;
	/* 0 means run until signaled, which is how the harness drives it. */
	double duration_s = (argc > 2) ? atof(argv[2]) : 0.0;

	/* Run where the compositor runs, so the measurement reflects what the UI
	 * would feel rather than what a background task feels. */
	if (pthread_set_qos_class_self_np(QOS_CLASS_USER_INTERACTIVE, 0) != 0) {
		fprintf(stderr, "uijitter: could not set user-interactive QoS\n");
		return 1;
	}

	struct sigaction sa;
	sa.sa_handler = on_signal;
	sigemptyset(&sa.sa_mask);
	sa.sa_flags = 0; /* no SA_RESTART: let nanosleep return so we can exit */
	sigaction(SIGINT, &sa, NULL);
	sigaction(SIGTERM, &sa, NULL);

	size_t capacity = INITIAL_CAPACITY, count = 0;
	double *samples = malloc(capacity * sizeof(double));
	if (samples == NULL) {
		fprintf(stderr, "uijitter: out of memory\n");
		return 1;
	}

	double start = now_ms();
	double deadline = start;

	while (!stop_flag) {
		if (duration_s > 0 && (now_ms() - start) / 1000.0 >= duration_s) break;
		deadline += period_ms;
		sleep_ms(deadline - now_ms());
		if (stop_flag) break;

		double overshoot = now_ms() - deadline;
		if (overshoot < 0) overshoot = 0;

		if (count == capacity) {
			size_t grown = capacity * 2;
			double *bigger = realloc(samples, grown * sizeof(double));
			if (bigger == NULL) break; /* report what we have */
			samples = bigger;
			capacity = grown;
		}
		samples[count++] = overshoot;

		/* A stall long enough to skip whole periods would otherwise make every
		 * subsequent wake look late; resynchronize to now. */
		if (now_ms() > deadline + period_ms) deadline = now_ms();
	}

	double elapsed_s = (now_ms() - start) / 1000.0;
	qsort(samples, count, sizeof(double), cmp_double);

	size_t visible = 0, severe = 0;
	for (size_t i = 0; i < count; i++) {
		if (samples[i] >= VISIBLE_STUTTER_MS) visible++;
		if (samples[i] >= SEVERE_STALL_MS) severe++;
	}

	/* Single line, field=value, so a shell harness can parse it directly. */
	printf("samples=%zu elapsed_s=%.1f p50_ms=%.2f p95_ms=%.2f p99_ms=%.2f max_ms=%.2f over_%gms=%zu over_%gms=%zu\n",
	       count, elapsed_s,
	       percentile(samples, count, 50.0),
	       percentile(samples, count, 95.0),
	       percentile(samples, count, 99.0),
	       count > 0 ? samples[count - 1] : 0.0,
	       VISIBLE_STUTTER_MS, visible,
	       SEVERE_STALL_MS, severe);

	free(samples);
	return 0;
}
