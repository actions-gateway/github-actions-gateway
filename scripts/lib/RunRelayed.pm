# RunRelayed — run a command as a child and forward termination signals to it.
#
# The lock launchers (serialize_heavy_build in common.sh, serialize_on_cluster in
# deploy/monitoring/preview/render.sh) hold an flock in perl and run the real
# work as a child. perl's own `system` leaves TERM and HUP at their default, so a
# signal to perl alone killed it, the kernel dropped the lock, and the work ran
# on outside it beside the next holder (Q1093).
#
# A signal is forwarded to the child's whole descendant tree, not the child
# alone: the child is a bash script, and bash killed by a signal leaves the
# command it was running (a `go test`, a lint) alive and still mid-section. The
# tree is walked rather than signalled as a process group because the child
# stays in perl's group, so a group signal, SIGKILL included, still reaches
# everything as it did before. SIGKILL to perl alone cannot be forwarded and
# still orphans.
#
#   perl -I"$REPO_ROOT/scripts/lib" -MRunRelayed -e 'exit run_relayed(@ARGV)' cmd args...
package RunRelayed;

use strict;
use warnings;
use POSIX ();
use Exporter 'import';

our @EXPORT = ('run_relayed');

my @SIGNALS = qw(HUP INT QUIT TERM);

# run_relayed(@cmd) — fork/exec @cmd, forward @SIGNALS to it until it exits, and
# return its status as a shell would report it: the exit code, 128+n for a
# signal death, or 255 when it could not be started.
sub run_relayed {
	my @cmd = @_;
	# Blocked across the fork, so a signal landing before the handlers below are
	# installed is delivered to them rather than lost or taken by the child early.
	my $set = POSIX::SigSet->new(map { POSIX->can("SIG$_")->() } @SIGNALS);
	my $old = POSIX::SigSet->new;
	POSIX::sigprocmask(POSIX::SIG_BLOCK(), $set, $old);
	my $pid = fork;
	if (defined $pid && $pid == 0) {
		$SIG{$_} = 'DEFAULT' for @SIGNALS;
		POSIX::sigprocmask(POSIX::SIG_SETMASK(), $old);
		exec { $cmd[0] } @cmd or POSIX::_exit(255);
	}
	# local $?: the handler can run after waitpid returns and before $? is read,
	# and the `ps` in descendants() would replace the child's status with its own.
	if (defined $pid) {
		$SIG{$_} = sub { local ($?, $!); kill $_[0], $pid, descendants($pid) } for @SIGNALS;
	}
	POSIX::sigprocmask(POSIX::SIG_SETMASK(), $old);
	return 255 unless defined $pid;
	# perl's waitpid resumes after running a handler, so one call waits it out.
	waitpid($pid, 0);
	my $rc = $?;
	return $rc & 127 ? 128 + ($rc & 127) : $rc >> 8;
}

# descendants($pid) — every live process below $pid, read off one `ps` snapshot.
sub descendants {
	my ($root) = @_;
	my %kids;
	for (`ps -A -o pid= -o ppid=`) {
		my ($p, $pp) = split;
		push @{ $kids{$pp} }, $p if defined $pp;
	}
	my @out;
	my @queue = ($root);
	while (@queue) {
		my @next = @{ $kids{ shift @queue } || [] };
		push @out, @next;
		push @queue, @next;
	}
	return @out;
}

1;
