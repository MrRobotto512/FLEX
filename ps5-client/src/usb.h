#pragma once

/* Movies and TV shows on a USB drive plugged into the console: the folders
 * named Movies and TV (or Films, TV Shows, Shows, Series; any capitals) at
 * the top of each drive, played straight from the drive without a server. */

typedef struct {
    char path[512];
    char title[200]; /* from the file or folder name */
    int season, episode; /* episodes: from S01E02 / 1x02 in the name, else 0 */
} UsbFile;

typedef struct {
    char title[200];
    UsbFile *eps; /* in season, episode, name order */
    int n;
} UsbShow;

/* usb_changed: 1 when the drives' Movies/TV folders differ from the last
 * scan (a drive went in or out). Cheap; meant to be asked every few seconds. */
int usb_changed(void);

/* usb_scan reads the drives again. The lists stay valid until the next scan. */
void usb_scan(void);
int usb_movie_count(void);
const UsbFile *usb_movie(int i);
int usb_show_count(void);
const UsbShow *usb_show(int i);

/* Where each file was stopped, kept in the app folder. Stopping in the last
 * 5 % counts as watched to the end and forgets the file. */
int usb_resume(const char *path);
void usb_set_resume(const char *path, double position, double duration);

/* usb_duration: the length in seconds of what usb_url gave, 0 if it can't be read. */
double usb_duration(const char *url);

/* usb_url writes what the player should open for a file on the drive: the
 * file itself, or a local web address when the app may not read the drive.
 * Returns 1 when the player can seek in it. */
#include <stddef.h>
int usb_url(const char *path, char *out, size_t size);
