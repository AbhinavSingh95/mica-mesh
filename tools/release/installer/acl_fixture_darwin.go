// This file is included only in the installer_test target.
package main

/*
#include <sys/acl.h>
#include <membership.h>
#include <unistd.h>
#include <errno.h>

static int fixture_acl(int fd, acl_tag_t tag, acl_perm_t permission, uid_t uid) {
    acl_t acl = acl_init(1);
    if (!acl) return -1;
    acl_entry_t entry;
    acl_permset_t permissions;
    uuid_t user;
    int result = -1;
    if (acl_create_entry(&acl, &entry) != 0 ||
        acl_set_tag_type(entry, tag) != 0 ||
        mbr_uid_to_uuid(uid, user) != 0 ||
        acl_set_qualifier(entry, user) != 0 ||
        acl_get_permset(entry, &permissions) != 0 ||
        acl_add_perm(permissions, permission) != 0 ||
        acl_set_permset(entry, permissions) != 0) goto done;
    result = acl_set_fd_np(fd, acl, ACL_TYPE_EXTENDED);
 done:;
    int saved = errno;
    if (acl_free(acl) != 0) return -1;
    errno = saved;
    return result;
}
*/
import "C"

import "os"

func fixtureWriteACL(file *os.File) error {
	result, err := C.fixture_acl(C.int(file.Fd()), C.ACL_EXTENDED_ALLOW, C.ACL_WRITE_DATA, C.getuid())
	if result != 0 {
		return err
	}
	return nil
}

func fixtureDenyACL(file *os.File, right string) error {
	permission := C.acl_perm_t(C.ACL_DELETE)
	switch right {
	case "read":
		permission = C.ACL_READ_DATA
	case "execute":
		permission = C.ACL_EXECUTE
	case "attributes":
		permission = C.ACL_READ_ATTRIBUTES
	}
	// A different uid leaves the test owner able to inspect the path, as root
	// could during installation, while an ordinary user would be denied.
	result, err := C.fixture_acl(C.int(file.Fd()), C.ACL_EXTENDED_DENY, permission, 65534)
	if result != 0 {
		return err
	}
	return nil
}
