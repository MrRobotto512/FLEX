#pragma once
#include <stddef.h>

/* LAN discovery: broadcasts FLEX_DISCOVER and fills host/port from the first
 * reply. Returns 0 on success, -1 on timeout/error. */
int flex_discover(char *host, size_t hostlen, int *port, int timeout_ms);

/* Minimal HTTP/1.0 client, enough for the FLEX API. Returns a malloc'd,
 * NUL-terminated body (len set if non-NULL) or NULL on error. Sends
 * X-Flex-Client: ps5 on every request. */
char *flex_http(const char *host, int port, const char *method, const char *path,
                const char *json_body, size_t *len);

#define flex_get(h, p, path) flex_http((h), (p), "GET", (path), NULL, NULL)
