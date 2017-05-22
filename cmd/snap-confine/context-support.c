/*
 * Copyright (C) 2017 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

#include "context-support.h"

#include "../libsnap-confine-private/cleanup-funcs.h"
#include "../libsnap-confine-private/string-utils.h"
#include "../libsnap-confine-private/utils.h"

#include "config.h"

#include <fcntl.h>
#include <string.h>
#include <sys/types.h>
#include <sys/stat.h>
#include <unistd.h>

#define SC_CONTEXT_DIR "/var/lib/snapd/context"

/**
 * Effective value of CONTEXT_DIR
 **/
static const char *sc_context_dir = SC_CONTEXT_DIR;

typedef char sc_context_cookie[45];

char *sc_context_get_from_snapd(const char *snap_name, struct sc_error **errorp)
{
	char context_path[PATH_MAX];
	char *context_val = NULL;
	struct sc_error *err = NULL;

	sc_must_snprintf(context_path, sizeof(context_path), "%s/snap.%s",
			 sc_context_dir, snap_name);
	int fd __attribute__ ((cleanup(sc_cleanup_close))) = -1;
	fd = open(context_path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
	if (fd < 0) {
		err =
		    sc_error_init(SC_ERRNO_DOMAIN, 0,
				  "cannot open context file %s, SNAP_CONTEXT will not be set",
				  context_path);
		goto out;
	}
	// context is a 32 bytes, base64-encoding makes it 44.
	context_val = calloc(1, sizeof(sc_context_cookie));
	if (context_val == NULL) {
		die("failed to allocate memory for snap context");
	}
	if (read(fd, context_val, sizeof(sc_context_cookie) - 1) < 0) {
		free(context_val);
		context_val = NULL;
		err =
		    sc_error_init(SC_ERRNO_DOMAIN, 0,
				  "failed to read context file %s",
				  context_path);
		goto out;
	}

 out:
	sc_error_forward(errorp, err);
	return context_val;
}

void sc_maybe_set_context_environment(const char *context)
{
	if (context != NULL) {
		// Overwrite context env value.
		setenv("SNAP_CONTEXT", context, 1);
	}
}
