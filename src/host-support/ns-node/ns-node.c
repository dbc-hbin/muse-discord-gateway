/* ns-node: exec Node.js as uid/gid 1000 inside a fresh user namespace whose
 * single mapping points at the real root account outside, with a small set of
 * ambient capabilities retained across the exec.
 *
 * Why: this VM runs everything as root, but Chromium refuses to run with its
 * sandbox as root, and the sandbox itself needs CAP_SYS_ADMIN/CAP_SETUID in
 * the browser process's user namespace to create its child namespaces and
 * write their uid maps. Mapping uid 1000 -> outer root keeps files created
 * inside the namespace root-owned outside (so the root-owned browser queue
 * still passes same-owner checks from both sides), while inside the
 * namespace the process is a non-root user with just enough ambient
 * capability for Chromium's sandbox to work.
 */
#define _GNU_SOURCE
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#include <linux/capability.h>

#ifndef PR_CAP_AMBIENT
#define PR_CAP_AMBIENT 47
#define PR_CAP_AMBIENT_RAISE 2
#endif

static void fail(const char *what) {
    perror(what);
    exit(1);
}

static void write_file(const char *path, const char *value) {
    FILE *f = fopen(path, "w");
    if (!f) fail(path);
    if (fputs(value, f) < 0) fail(path);
    if (fclose(f) != 0) fail(path);
}

int main(int argc, char **argv) {
    /* SETGID, SETUID, SYS_CHROOT, SYS_ADMIN */
    static const int caps[] = {CAP_SETGID, CAP_SETUID, CAP_SYS_CHROOT, CAP_SYS_ADMIN};
    const int ncaps = (int)(sizeof(caps) / sizeof(caps[0]));

    if (unshare(CLONE_NEWUSER) != 0) fail("unshare(CLONE_NEWUSER)");
    /* We are real root in the parent namespace, so we may write our own maps. */
    write_file("/proc/self/setgroups", "deny");
    write_file("/proc/self/uid_map", "1000 0 1\n");
    write_file("/proc/self/gid_map", "1000 0 1\n");

    struct __user_cap_header_struct hdr = {_LINUX_CAPABILITY_VERSION_3, 0};
    struct __user_cap_data_struct data[2] = {{0, 0, 0}, {0, 0, 0}};
    if (syscall(SYS_capget, &hdr, &data) != 0) fail("capget");
    unsigned int mask = 0;
    for (int i = 0; i < ncaps; i++) mask |= (1u << caps[i]);
    data[0].inheritable |= mask;
    if (syscall(SYS_capset, &hdr, &data) != 0) fail("capset inheritable");
    for (int i = 0; i < ncaps; i++) {
        if (prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_RAISE, (unsigned long)caps[i], 0, 0) != 0)
            fail("prctl(PR_CAP_AMBIENT_RAISE)");
    }

    char **node_argv = malloc(sizeof(char *) * (size_t)(argc + 1));
    if (!node_argv) fail("malloc");
    node_argv[0] = "node";
    for (int i = 1; i < argc; i++) node_argv[i] = argv[i];
    node_argv[argc] = NULL;
    execvp("/usr/bin/node", node_argv);
    fail("exec /usr/bin/node");
    return 1;
}
