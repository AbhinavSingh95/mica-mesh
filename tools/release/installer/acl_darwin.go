package main

/*
#include <sys/acl.h>
#include <errno.h>

// Permission bits alone do not describe macOS directory access. Inspect the
// held descriptor, so an ACL cannot permit another user to race activation.
static int check_acl(int fd, int public_access) {
    acl_t acl = acl_get_fd_np(fd, ACL_TYPE_EXTENDED);
    if (acl == NULL) {
        if (errno == ENOENT) return 0;
        return -1;
    }
    acl_entry_t entry;
    int position = ACL_FIRST_ENTRY;
    const acl_perm_t writes[] = { ACL_WRITE_DATA, ACL_APPEND_DATA, ACL_DELETE,
        ACL_DELETE_CHILD, ACL_WRITE_ATTRIBUTES, ACL_WRITE_EXTATTRIBUTES,
        ACL_WRITE_SECURITY, ACL_CHANGE_OWNER };
    const acl_perm_t reads[] = { ACL_READ_DATA, ACL_EXECUTE, ACL_READ_ATTRIBUTES };
    while (acl_get_entry(acl, position, &entry) == 0) {
        position = ACL_NEXT_ENTRY;
        acl_tag_t tag;
        acl_permset_t permissions;
        if (acl_get_tag_type(entry, &tag) != 0 ||
            acl_get_permset(entry, &permissions) != 0) {
            int saved = errno; acl_free(acl); errno = saved; return -1;
        }
        const acl_perm_t *rights;
        unsigned int count;
        int unsafe;
        if (tag == ACL_EXTENDED_ALLOW) {
            rights = writes; count = sizeof(writes)/sizeof(writes[0]); unsafe = 1;
        } else if (public_access && tag == ACL_EXTENDED_DENY) {
            // Conservative: do not try to evaluate principals or inheritance.
            // Deny-delete remains safe; deny-read/list/search/execute/stat does not.
            rights = reads; count = sizeof(reads)/sizeof(reads[0]); unsafe = 2;
        } else {
            continue;
        }
        for (unsigned int i = 0; i < count; i++) {
            int present = acl_get_perm_np(permissions, rights[i]);
            if (present < 0) {
                int saved = errno; acl_free(acl); errno = saved; return -1;
            }
            if (present) { acl_free(acl); return unsafe; }
        }
    }
    // Darwin uses EINVAL to mark the end of a valid ACL's entry list.
    int saved = errno;
    if (acl_free(acl) != 0) return -1;
    if (saved != EINVAL) { errno = saved; return -1; }
    return 0;
}
*/
import "C"

import (
	"fmt"
	"os"
)

func checkACL(file *os.File, public bool) error {
	var publicAccess C.int
	if public {
		publicAccess = 1
	}
	result, err := C.check_acl(C.int(file.Fd()), publicAccess)
	if result < 0 {
		return fmt.Errorf("inspect filesystem ACL at %s: %w", file.Name(), err)
	}
	if result == 1 {
		return fmt.Errorf("write access is granted by an ACL at %s; ask the administrator to review this directory before installing", file.Name())
	}
	if result == 2 {
		return fmt.Errorf("public access is denied by an ACL at %s; ask the administrator to review its access permissions before installing", file.Name())
	}
	return nil
}
