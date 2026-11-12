/* What app/start.c and app/heap.c offer the rest of FLEX when it runs as its
 * own PS5 app (built with FLEX_APP). */

#ifndef FLEX_APP_H
#define FLEX_APP_H

#include <stddef.h>

/* app_unbound lists the system functions FLEX uses that the app loader left
 * unbound ("" when all are bound). */
void app_unbound(char *out, size_t size);

/* app_heap_desc says where FLEX's own heap came from and how big it is. */
const char *app_heap_desc(void);

/* app_heap_mb: FLEX's heap in use, taken from its memory so far, and its
 * whole size, in MB. -1 if there is no such heap. */
int app_heap_mb(long *inuse, long *system, long *cap);

#endif
