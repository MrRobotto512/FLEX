/*
 * FLEX's home-screen tile: the app the tile opens. An app may not start
 * another app on the PS5, so it asks websrv's homebrew loader (a payload,
 * which may) to start FLEX: GET 127.0.0.1:8080/hbldr, the same request the
 * Homebrew Launcher page and FLEX's own updater send. The loader closes
 * this app and runs FLEX in its homebrew host, so this one only waits.
 *
 * Built with the native-app tools of Forwarder Manager
 * (github.com/heydemoura/ps5-forwarder-manager) and modelled on its
 * forwarder program (ps5-forwarder-format, forwarder/src/main.c).
 * SPDX-License-Identifier: GPL-3.0-or-later
 *
 * Steps go to the console log and to /data/homebrew/FLEX/tile.log; a failure
 * also raises a notification.
 */
#include <arpa/inet.h>
#include <netinet/in.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

#define FLEX_ELF "/data/homebrew/FLEX/eboot.elf"
#define TILE_LOG "/data/homebrew/FLEX/tile.log"
#define LOADER_PORT 8080

struct notification {
    unsigned char reserved[45];
    char message[3075];
};

int sceKernelSendNotificationRequest(unsigned int device, void *request, size_t size, int blocking);
int sceKernelDebugOutText(int channel, const char *text);

static void log_line(const char *format, ...) __attribute__((format(printf, 1, 2)));
static void log_line(const char *format, ...) {
    char text[512], line[600];
    va_list args;
    va_start(args, format);
    (void)vsnprintf(text, sizeof text, format, args);
    va_end(args);
    (void)snprintf(line, sizeof line, "[FLEX tile] %s\n", text);
    (void)sceKernelDebugOutText(0, line);
    FILE *f = fopen(TILE_LOG, "a");
    if (f) {
        fputs(line, f);
        fclose(f);
    }
}

static void notify(const char *message) {
    static struct notification request;
    memset(&request, 0, sizeof request);
    (void)snprintf(request.message, sizeof request.message, "%s", message);
    (void)sceKernelSendNotificationRequest(0, &request, sizeof request, 0);
}

/* ask_loader sends the start request and reads the answer; 0 once sent. */
static int ask_loader(void) {
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) {
        log_line("socket failed");
        return -1;
    }
    struct timeval timeout = {30, 0}; /* the loader answers once FLEX is up */
    (void)setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof timeout);
    struct sockaddr_in address;
    memset(&address, 0, sizeof address);
    address.sin_family = AF_INET;
    address.sin_port = htons(LOADER_PORT);
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (connect(fd, (struct sockaddr *)&address, sizeof address) != 0) {
        log_line("no homebrew loader on 127.0.0.1:%d", LOADER_PORT);
        close(fd);
        return -1;
    }
    static const char request[] =
        "GET /hbldr?path=" FLEX_ELF "&pipe=0&daemon=0 HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n";
    if (write(fd, request, sizeof request - 1) != (ssize_t)(sizeof request - 1)) {
        log_line("sending the request failed");
        close(fd);
        return -1;
    }
    log_line("asked the loader to start " FLEX_ELF);
    char answer[256];
    ssize_t n = read(fd, answer, sizeof answer - 1);
    if (n > 0) {
        answer[n] = '\0';
        char *end = strpbrk(answer, "\r\n");
        if (end) *end = '\0';
        log_line("loader answered: %s", answer);
    }
    close(fd);
    return 0;
}

int main(void) {
    log_line("started");
    for (int tries = 0; tries < 3; ++tries) {
        if (ask_loader() == 0) {
            /* the loader closes this app as it starts FLEX */
            for (int i = 0; i < 200; ++i) usleep(100 * 1000);
            log_line("still open after 20 s; closing");
            return 0;
        }
        sleep(2);
    }
    notify("FLEX: the homebrew loader isn't running. Load the jailbreak, then open FLEX again.");
    return 1;
}
