/* pskill: a tiny elfldr payload for a console whose FLEX has hung (a second
 * launch over a running copy jams the screen; the PS button stops working).
 * Lists the processes it can see, and with a name as the argument SIGKILLs
 * every process of that name except itself. Sent headless to elfldr (port
 * 9021), its output comes back on the same connection:
 *
 *   make -f tools/pskill.mk && nc -q0 PS5 9021 < build/pskill.elf          # list
 *   ... "file:/user/temp/pskill.elf?args=eboot.elf" via PS5_DEPLOY           # kill
 */
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/sysctl.h>
#include <sys/types.h>
#include <sys/user.h>
#include <unistd.h>

int main(int argc, char *argv[]) {
    int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PROC, 0};
    size_t len = 0;
    if (sysctl(mib, 3, NULL, &len, NULL, 0) < 0) {
        perror("sysctl size");
        return 1;
    }
    struct kinfo_proc *procs = malloc(len + 4096);
    if (sysctl(mib, 3, procs, &len, NULL, 0) < 0) {
        perror("sysctl");
        return 1;
    }
    const char *target = argc > 1 ? argv[1] : NULL;
    int killed = 0;
    for (size_t i = 0; i < len / sizeof *procs; i++) {
        struct kinfo_proc *p = &procs[i];
        printf("%6d %6d %-20s\n", p->ki_pid, p->ki_ppid, p->ki_comm);
        if (target && strcmp(p->ki_comm, target) == 0 && p->ki_pid != getpid()) {
            if (kill(p->ki_pid, SIGKILL) == 0) {
                printf("killed %d (%s)\n", p->ki_pid, p->ki_comm);
                killed++;
            } else {
                perror("kill");
            }
        }
    }
    if (target) printf("%d process(es) named %s killed\n", killed, target);
    return 0;
}
