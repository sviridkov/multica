// Native kernel-filtered RUID census for the closed disposable task domain.
// Failure and capacity exhaustion never mean an empty domain.
#include <errno.h>
#include <libproc.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

int main(int argc, char **argv) {
    if (argc != 2 || geteuid() != 0) return 2;
    char *end = NULL;
    errno = 0;
    unsigned long uid = strtoul(argv[1], &end, 10);
    if (errno || !end || *end || uid == 0 || uid > 0xffffffffUL) return 2;
    const int bytes = 65536 * (int)sizeof(pid_t);
    pid_t *pids = calloc(1, bytes);
    if (!pids) return 2;
    errno = 0;
    int got = proc_listpids(PROC_RUID_ONLY, (uint32_t)uid, pids, bytes);
    if (got < 0 || (got == 0 && errno) || got >= bytes || got % sizeof(pid_t)) {
        free(pids);
        return 3;
    }
    printf("%d\n", got / (int)sizeof(pid_t));
    free(pids);
    return 0;
}
