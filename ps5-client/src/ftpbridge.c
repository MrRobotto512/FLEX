/* A bridge from the console's FTP server to HTTP, for playing files on a USB
 * drive.
 *
 * As its own app FLEX may not read the drive, the jailbreak's web server
 * (websrv) can't serve part of a file, and FFmpeg's FTP reader loses step
 * with the jailbreak's FTP server (ftpsrv answers the abort FFmpeg sends
 * with replies it doesn't expect). ftpsrv itself is fine: it can start a
 * file anywhere (REST). So FLEX listens on a local port and answers each
 * HTTP request, byte range and all, by fetching that part over a fresh FTP
 * connection; the player reads http://127.0.0.1:<port>/<path> like any other
 * stream, seeking included. */

#include "ftpbridge.h"

#include <arpa/inet.h>
#include <ctype.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

#include "blackbox.h"

static char ftp_host[64];
static int ftp_port, http_port;

static void quiet(int fd) { /* a closed peer must not kill the app with SIGPIPE */
#ifdef SO_NOSIGPIPE
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &(int){1}, sizeof(int));
#else
    (void)fd;
#endif
}

#ifndef MSG_NOSIGNAL
#define MSG_NOSIGNAL 0
#endif

static int send_all(int fd, const char *buf, size_t len) {
    while (len) {
        ssize_t n = send(fd, buf, len, MSG_NOSIGNAL);
        if (n <= 0) return -1;
        buf += n, len -= (size_t)n;
    }
    return 0;
}

static int dial(const char *host, int port) {
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) return -1;
    struct sockaddr_in a = {.sin_family = AF_INET, .sin_port = htons((unsigned short)port)};
    struct timeval tv = {15, 0};
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
    quiet(fd);
    if (inet_pton(AF_INET, host, &a.sin_addr) != 1 || connect(fd, (struct sockaddr *)&a, sizeof a) != 0) {
        close(fd);
        return -1;
    }
    return fd;
}

/* reply reads one FTP reply (its last line: "NNN text") and returns NNN. */
static int reply(int fd, char *text, size_t size) {
    char line[512];
    for (;;) {
        size_t n = 0;
        char c;
        while (n + 1 < sizeof line) {
            if (recv(fd, &c, 1, 0) != 1) return -1;
            if (c == '\n') break;
            if (c != '\r') line[n++] = c;
        }
        line[n] = 0;
        if (n >= 4 && isdigit((unsigned char)line[0]) && isdigit((unsigned char)line[1]) &&
            isdigit((unsigned char)line[2]) && line[3] == ' ') {
            if (text) snprintf(text, size, "%s", line + 4);
            return atoi(line);
        }
    }
}

static int command(int fd, char *text, size_t size, const char *fmt, const char *arg) {
    char line[1200];
    int n = snprintf(line, sizeof line - 2, fmt, arg);
    if (n < 0 || n >= (int)sizeof line - 2) return -1;
    memcpy(line + n, "\r\n", 2);
    if (send_all(fd, line, (size_t)n + 2) != 0) return -1;
    return reply(fd, text, size);
}

/* ftp_fetch opens path at byte `start`: the control and data connections,
 * and the file's size. Returns 0, or an HTTP status to answer with. */
static int ftp_fetch(const char *path, long long start, int *ctl, int *data, long long *size) {
    char text[512];
    *ctl = *data = -1;
    int c = dial(ftp_host, ftp_port);
    if (c < 0) return 502;
    int code = reply(c, NULL, 0);
    if (code == 220) code = command(c, NULL, 0, "USER %s", "anonymous");
    if (code == 331) code = command(c, NULL, 0, "PASS %s", "flex");
    if (code != 230 || command(c, NULL, 0, "TYPE %s", "I") != 200) goto bad;
    if (command(c, text, sizeof text, "SIZE %s", path) != 213) {
        close(c);
        return 404;
    }
    *size = atoll(text);
    if (start >= *size) {
        close(c);
        return 416;
    }
    if (command(c, text, sizeof text, "PASV%s", "") != 227) goto bad;
    int h1, h2, h3, h4, p1, p2;
    const char *nums = strchr(text, '(');
    if (!nums || sscanf(nums, "(%d,%d,%d,%d,%d,%d", &h1, &h2, &h3, &h4, &p1, &p2) != 6) goto bad;
    int d = dial(ftp_host, p1 << 8 | p2); /* the server's own address, whatever it says */
    if (d < 0) goto bad;
    char at[32];
    snprintf(at, sizeof at, "%lld", start);
    if ((start > 0 && command(c, NULL, 0, "REST %s", at) != 350) || command(c, NULL, 0, "RETR %s", path) / 100 != 1) {
        close(d);
        goto bad;
    }
    *ctl = c, *data = d;
    return 0;
bad:
    close(c);
    return 502;
}

static void unescape(char *s) {
    char *o = s;
    for (; *s; s++) {
        if (s[0] == '%' && isxdigit((unsigned char)s[1]) && isxdigit((unsigned char)s[2])) {
            char hex[3] = {s[1], s[2], 0};
            *o++ = (char)strtol(hex, NULL, 16);
            s += 2;
        } else {
            *o++ = *s;
        }
    }
    *o = 0;
}

static void *serve(void *arg) {
    int fd = (int)(long)arg;
    struct timeval tv = {30, 0};
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
    quiet(fd);

    char req[4096] = ""; /* empty: the loop below looks for the end of the headers in it */
    size_t len = 0;
    while (len + 1 < sizeof req && !strstr(req, "\r\n\r\n")) {
        ssize_t n = recv(fd, req + len, sizeof req - 1 - len, 0);
        if (n <= 0) goto done;
        len += (size_t)n;
        req[len] = 0;
    }
    char path[1024] = "";
    if (sscanf(req, "GET %1023s", path) != 1 || path[0] != '/') goto done;
    unescape(path);
    long long start = 0, end = -1;
    int ranged = 0;
    for (char *h = req; (h = strstr(h, "\r\n")); h += 2)
        if (strncasecmp(h + 2, "Range:", 6) == 0) ranged = sscanf(h + 8, " bytes=%lld-%lld", &start, &end) >= 1;

    int ctl, data;
    long long size = 0;
    int status = ftp_fetch(path, start, &ctl, &data, &size);
    char head[400];
    if (status) {
        int n = snprintf(head, sizeof head, "HTTP/1.1 %d Error\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status);
        send_all(fd, head, (size_t)n);
        goto done;
    }
    if (end < 0 || end >= size) end = size - 1;
    long long left = end - start + 1;
    int n = snprintf(head, sizeof head,
                     "HTTP/1.1 %s\r\nContent-Type: application/octet-stream\r\nAccept-Ranges: bytes\r\n"
                     "Content-Length: %lld\r\nContent-Range: bytes %lld-%lld/%lld\r\nConnection: close\r\n\r\n",
                     ranged ? "206 Partial Content" : "200 OK", left, start, end, size);
    if (send_all(fd, head, (size_t)n) == 0) {
        static const size_t chunk = 256 * 1024;
        char *buf = malloc(chunk);
        while (buf && left > 0) {
            ssize_t got = recv(data, buf, left < (long long)chunk ? (size_t)left : chunk, 0);
            if (got <= 0 || send_all(fd, buf, (size_t)got) != 0) break; /* the player went elsewhere */
            left -= got;
        }
        free(buf);
    }
    close(data); /* a fresh connection next time: nothing to keep in step */
    close(ctl);
done:
    close(fd);
    return NULL;
}

static void *listener(void *arg) {
    int ls = (int)(long)arg;
    for (;;) {
        int fd = accept(ls, NULL, NULL);
        if (fd < 0) continue;
        pthread_t t;
        pthread_attr_t at;
        pthread_attr_init(&at);
        pthread_attr_setdetachstate(&at, PTHREAD_CREATE_DETACHED);
        if (pthread_create(&t, &at, serve, (void *)(long)fd) != 0) close(fd);
        pthread_attr_destroy(&at);
    }
    return NULL;
}

int ftpbridge_port(const char *host, int port, int any_address) {
    if (http_port) return http_port;
    snprintf(ftp_host, sizeof ftp_host, "%s", host);
    ftp_port = port;
    int ls = socket(AF_INET, SOCK_STREAM, 0);
    if (ls < 0) return 0;
    setsockopt(ls, SOL_SOCKET, SO_REUSEADDR, &(int){1}, sizeof(int));
    struct sockaddr_in a = {.sin_family = AF_INET};
    a.sin_addr.s_addr = htonl(any_address ? INADDR_ANY : INADDR_LOOPBACK);
    socklen_t alen = sizeof a;
    pthread_t t;
    if (bind(ls, (struct sockaddr *)&a, sizeof a) != 0 || listen(ls, 8) != 0 ||
        getsockname(ls, (struct sockaddr *)&a, &alen) != 0 ||
        pthread_create(&t, NULL, listener, (void *)(long)ls) != 0) {
        close(ls);
        return 0;
    }
    pthread_detach(t);
    http_port = ntohs(a.sin_port);
    bb_log("usb: reading the drive through the console's FTP server (%s:%d), bridged on port %d", host, port, http_port);
    return http_port;
}
