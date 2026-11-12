#pragma once

/* The black box: a log on the console's disk that survives a freeze. Each
 * line goes straight to the file and is synced, so after a hard restart the
 * last lines show what the app was doing and how the console was coping
 * (memory, CPU, how long drawing took) right up to the moment it locked up.
 * Lines also go to stderr (websrv's pipe, when there is one).
 *
 * /data/homebrew/FLEX/blackbox.txt is this run; blackbox.1.txt and
 * blackbox.2.txt are the two runs before it (a freeze's log is kept even if
 * FLEX is started again before anyone reads it). */

void bb_open(const char *version);

/* bb_log writes one line (no newline needed), prefixed with the seconds since start. */
void bb_log(const char *fmt, ...) __attribute__((format(printf, 1, 2)));

/* bb_system fills buf with how the console is doing: this process's CPU use
 * and page faults since the last call, the whole console's CPU use, memory
 * this app holds (malloc's own figures) and the console's free memory.
 * Figures the system won't give show as "?". */
void bb_system(char *buf, int size);

/* bb_heap_mb: malloc's own figures, what the app has in use and what malloc
 * has taken from the system for it (much more, when big blocks come and go
 * between small long-lived ones). The system's "flexible memory" figure
 * (448 MB) does not follow either. Returns -1 if the system can't say. */
int bb_heap_mb(long *inuse, long *system);

/* bb_every logs "<what> <system figures>" if ms have passed since the last such line. */
void bb_every(unsigned ms, const char *what);
