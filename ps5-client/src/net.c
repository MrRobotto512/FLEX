#include "net.h"

#include <arpa/inet.h>
#include <netdb.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/select.h>
#include <sys/socket.h>
#include <unistd.h>

#include "cJSON.h"

#define DISCOVERY_PORT 7555
#define DISCOVERY_MSG "FLEX_DISCOVER 1"

int flex_discover(char *host, size_t hostlen, int *port, int timeout_ms) {
    int s = socket(AF_INET, SOCK_DGRAM, 0);
    if (s < 0) return -1;
    int yes = 1;
    setsockopt(s, SOL_SOCKET, SO_BROADCAST, &yes, sizeof yes);

    struct sockaddr_in to = {.sin_family = AF_INET, .sin_port = htons(DISCOVERY_PORT)};
    to.sin_addr.s_addr = htonl(INADDR_BROADCAST);
    if (sendto(s, DISCOVERY_MSG, sizeof DISCOVERY_MSG - 1, 0, (struct sockaddr *)&to, sizeof to) < 0) {
        close(s);
        return -1;
    }

    fd_set rd;
    FD_ZERO(&rd);
    FD_SET(s, &rd);
    struct timeval tv = {.tv_sec = timeout_ms / 1000, .tv_usec = (timeout_ms % 1000) * 1000};
    if (select(s + 1, &rd, NULL, NULL, &tv) <= 0) {
        close(s);
        return -1;
    }

    char buf[512];
    struct sockaddr_in from;
    socklen_t fl = sizeof from;
    ssize_t n = recvfrom(s, buf, sizeof buf - 1, 0, (struct sockaddr *)&from, &fl);
    close(s);
    if (n <= 0) return -1;
    buf[n] = 0;

    cJSON *j = cJSON_Parse(buf);
    if (!j) return -1;
    cJSON *p = cJSON_GetObjectItem(j, "port");
    *port = cJSON_IsNumber(p) ? p->valueint : 8484;
    cJSON_Delete(j);
    inet_ntop(AF_INET, &from.sin_addr, host, hostlen);
    return 0;
}

static int tcp_connect(const char *host, int port) {
    char portstr[8];
    snprintf(portstr, sizeof portstr, "%d", port);
    struct addrinfo hints = {.ai_family = AF_INET, .ai_socktype = SOCK_STREAM}, *res;
    if (getaddrinfo(host, portstr, &hints, &res) != 0) return -1;
    int s = -1;
    for (struct addrinfo *a = res; a; a = a->ai_next) {
        s = socket(a->ai_family, a->ai_socktype, a->ai_protocol);
        if (s < 0) continue;
        if (connect(s, a->ai_addr, a->ai_addrlen) == 0) break;
        close(s);
        s = -1;
    }
    freeaddrinfo(res);
    return s;
}

char *flex_http(const char *host, int port, const char *method, const char *path,
                const char *json_body, size_t *len) {
    int s = tcp_connect(host, port);
    if (s < 0) return NULL;

    char req[2048];
    int n = snprintf(req, sizeof req,
                     "%s %s HTTP/1.0\r\nHost: %s:%d\r\nX-Flex-Client: ps5\r\nConnection: close\r\n",
                     method, path, host, port);
    if (json_body)
        n += snprintf(req + n, sizeof req - n, "Content-Type: application/json\r\nContent-Length: %zu\r\n",
                      strlen(json_body));
    n += snprintf(req + n, sizeof req - n, "\r\n");
    if (send(s, req, n, 0) < 0 || (json_body && send(s, json_body, strlen(json_body), 0) < 0)) {
        close(s);
        return NULL;
    }

    size_t cap = 16384, used = 0;
    char *buf = malloc(cap);
    for (;;) {
        if (used + 4096 > cap) buf = realloc(buf, cap *= 2);
        ssize_t r = recv(s, buf + used, cap - used - 1, 0);
        if (r <= 0) break;
        used += r;
    }
    close(s);
    buf[used] = 0;

    /* status line */
    int status = 0;
    sscanf(buf, "HTTP/%*d.%*d %d", &status);
    char *body = strstr(buf, "\r\n\r\n");
    if (!body || status < 200 || status >= 300) {
        fprintf(stderr, "flex_http: %s %s -> HTTP %d\n", method, path, status);
        free(buf);
        return NULL;
    }
    body += 4;
    size_t blen = used - (body - buf);
    memmove(buf, body, blen + 1);
    if (len) *len = blen;
    return buf;
}
