#pragma once

/* ftpbridge_port starts (once) a local HTTP server that serves files from
 * the FTP server at host:port, byte ranges included, and returns its port
 * (0 if it couldn't start): http://127.0.0.1:<port>/<url-escaped path>.
 * any_address: listen on every address instead of this machine's only (the
 * test on a PC). */
int ftpbridge_port(const char *host, int port, int any_address);
