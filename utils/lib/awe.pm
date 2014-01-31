package AWE;

use strict;
use warnings;
no warnings('once');

use File::Basename;
use Data::Dumper;
use JSON;
use LWP::UserAgent;

1;

sub new {
    my ($class, $awe_url, $shocktoken) = @_;
    
    my $agent = LWP::UserAgent->new;
    my $json = JSON->new;
    $json = $json->utf8();
    $json->max_size(0);
    $json->allow_nonref;
    
    my $self = {
        json => $json,
        agent => $agent,
        awe_url => $awe_url || '',
        shocktoken => $shocktoken || '',
        transport_method => 'requests'
    };
    if (system("type shock-client > /dev/null 2>&1") == 0) {
        $self->{transport_method} = 'shock-client';
    }

    bless $self, $class;
    return $self;
}

sub json {
    my ($self) = @_;
    return $self->{json};
}
sub agent {
    my ($self) = @_;
    return $self->{agent};
}
sub awe_url {
    my ($self) = @_;
    return $self->{awe_url};
}
sub shocktoken {
    my ($self) = @_;
    return $self->{shocktoken};
}
sub transport_method {
    my ($self) = @_;
    return $self->{transport_method};
}

# example: getJobQueue('info.clientgroups' => 'yourclientgroup')
sub getJobQueue {
	my ($self) = shift;
	my @pairs = @_;
	my $client_url = $self->awe_url.'/job';
	
	#print "got: ".join(',', @pairs)."\n";
	
	if (@pairs > 0) {
		$client_url .= '?query';
		for (my $i = 0 ; $i < @pairs ; $i+=2) {
			$client_url .= '&'.$pairs[$i].'='.$pairs[$i+1];
		}
	}
	
	my $http_response = $self->agent->get($client_url);
	my $http_response_content  = $http_response->decoded_content;
	die "Can't get url $client_url ", $http_response->status_line
	unless $http_response->is_success;
	
	my $http_response_content_hash = $self->json->decode($http_response_content);
	
	return $http_response_content_hash;
}

sub deleteJob {
	my ($self, $job_id) = @_;
	
	my $deletejob_url = $self->awe_url.'/job/'.$job_id;
	
	my $respond_content=undef;
	eval {
        
		my $http_response = $self->agent->delete( $deletejob_url );
		$respond_content = $self->json->decode( $http_response->content );
	};
	if ($@ || (! ref($respond_content))) {
        print STDERR "[error] unable to connect to AWE ".$self->awe_url."\n";
        return undef;
    } elsif (exists($respond_content->{error}) && $respond_content->{error}) {
        print STDERR "[error] unable to delete job from AWE: ".$respond_content->{error}[0]."\n";
    } else {
        return $respond_content;
    }
}

sub getClientList {
	my ($self) = @_;
	
	unless (defined $self->awe_url) {
		die;
	}
	
	my $client_url = $self->awe_url.'/client/';
	my $http_response = $self->agent->get($client_url);
	
	my $http_response_content  = $http_response->decoded_content;
	#print "http_response_content: ".$http_response_content."\n";
	
	die "Can't get url $client_url ", $http_response->status_line
	unless $http_response->is_success;
	

	my $http_response_content_hash = $self->json->decode($http_response_content);

	return $http_response_content_hash;
}

# submit json_file or json_data
sub submit_job {
	my ($self, %hash) = @_;
	
	 my $content = {};
	if (defined $hash{json_file}) {
		unless (-s $hash{json_file}) {
			die "file not found";
		}
		$content->{upload} = [$hash{json_file}]
	}
	if (defined $hash{json_data}) {
		#print "upload: ".$hash{json_data}."\n";
		$content->{upload} = [undef, "n/a", Content => $hash{json_data}]
	}
	
	#my $content = {upload => [undef, "n/a", Content => $awe_qiime_job_json]};
	my $job_url = $self->awe_url.'/job';
	
	my $respond_content=undef;
	eval {
        
		my $http_response = $self->agent->post( $job_url, Datatoken => $self->shocktoken, Content_Type => 'multipart/form-data', Content => $content );
		$respond_content = $self->json->decode( $http_response->content );
	};
	if ($@ || (! ref($respond_content))) {
        print STDERR "[error] unable to connect to AWE ".$self->awe_url."\n";
        return undef;
    } elsif (exists($respond_content->{error}) && $respond_content->{error}) {
        print STDERR "[error] unable to post data to AWE: ".$respond_content->{error}[0]."\n";
    } else {
        return $respond_content;
    }
}

sub getJobStatus {
	my ($self, $job_id) = @_;
	
	
	my $jobstatus_url = $self->awe_url.'/job/'.$job_id;
	
	my $respond_content=undef;
	eval {
        
		my $http_response = $self->agent->get( $jobstatus_url );
		$respond_content = $self->json->decode( $http_response->content );
	};
	if ($@ || (! ref($respond_content))) {
        print STDERR "[error] unable to connect to AWE ".$self->awe_url."\n";
        return undef;
    } elsif (exists($respond_content->{error}) && $respond_content->{error}) {
        print STDERR "[error] unable to get data from AWE: ".$respond_content->{error}[0]."\n";
    } else {
        return $respond_content;
    }
}


sub checkClientGroup {
	my ($self, $clientgroup) = @_;
	
	my $client_list_hash = $self->getClientList();
	#print Dumper($client_list_hash);
	
	print "\nOther clients:\n";
	my $found_active_clients = 0;
	my $other_clients = 0;
	foreach my $client ( @{$client_list_hash->{'data'}} ) {
		unless (defined($client->{group}) && ($client->{group} eq $clientgroup)) {
			print $client->{name}." (".$client->{Status}.")  group: ".$client->{group}."  apps: ".join(',',@{$client->{apps}})."\n";
			$other_clients++;
		}
	}
	if ($other_clients == 0) {
		print "none.\n";
	}
	
	print "\nClients in clientgroup \"$clientgroup\":\n";
	foreach my $client ( @{$client_list_hash->{'data'}} ) {
		
		
		
		if (defined($client->{group}) && ($client->{group} eq $clientgroup)) {
			print $client->{name}." (".$client->{Status}.")  group: ".$client->{group}."  apps: ".join(',',@{$client->{apps}})."\n";
			
			if (lc($client->{Status}) eq 'active') {
				$found_active_clients++;
			} else {
				print "warning: client not active:\n";
			}
		}
	}
	
	
	if ($found_active_clients == 0) {
		print STDERR "warning: did not find any active client for clientgroup $clientgroup\n";
		return 1;
	}
	
	print "Summary: found $found_active_clients active client for clientgroup $clientgroup\n";
	return 0;
}

1;

##########################################
package AWE::Job;
use Data::Dumper;
use Storable qw(dclone);
use File::Basename;

1;


sub new {
    my ($class, %h) = @_;
	
	my $self = {
		
		'data' => {'info' => $h{'info'}},
		'tasks' => $h{'tasks'},
		
		
		#'trojan' => $h{'trojan'},
		'shockhost' => $h{'shockhost'},
		'task_templates' => $h{'task_templates'}
	};
	
	
		
	
	
	#assignTasks($self, %{$h{'job_input'}});
	print "A\n".Dumper($self->{'data'});
	assignTasks($self);
	print "B\n".Dumper($self->{'data'});
	replace_taskids($self);
	#delete $self->{'trojan'};
	#delete $self->{'shockhost'};
	#delete $self->{'task_templates'};
		
	
	bless $self, $class;
    return $self;
}

sub create_simple_template {
	my $cmd = shift(@_);
	#get outputs
	
	#print "cmd1: $cmd\n";
	my @outputs = $cmd =~ /@@(\S+)/g;
	$cmd =~ s/\@\@(\S+)/$1/;
	
	my @inputs = $cmd =~ /[^@]@(\S+)/g;

	#print "outputs: ".join(' ', @outputs)."\n";
	#print "inputs: ".join(' ', @inputs)."\n";
	#print "cmd2: $cmd\n";
	
	my $meta_template = {
		"cmd" => $cmd,
		"inputs" => \@inputs,
		"outputs" => \@outputs,
		"trojan" => {} # creates trojan script with basic features
	};
	
	
	return $meta_template;
}

sub createTask {
	my ($self, %h)= @_;
	
	
	my $taskid = $h{'task_id'};
	my $task_templates = $self->{'task_templates'};
	my $task_template_name = $h{'task_template'};
	my $task_cmd = $h{'task_cmd'};
	
	
	
	
	
	my $task_template=undef;
	my $task;
	
	if (defined $task_template_name) {
		
		
		
		my $tmpl = $task_templates->{$task_template_name};
		unless (defined $tmpl) {
			
			#print Dumper($task_templates);
			
			die "template \"$task_template_name\" not found";
		}
		$task_template = dclone($tmpl);
		print "use task template\n";
	} elsif (defined $task_cmd) {
		$task_template = create_simple_template($task_cmd);
		print "use simple task\n";
		#print Dumper($task)."\n";
	} else {
		print Dumper(%h)."\n";
		die "no task template found (task_template or task_cmd)";
	}
	
	#print "template:\n";
	#print Dumper($task_template)."\n";
	
	my $host = $h{'shockhost'} || $self->{'shockhost'};
	
	$task->{'totalwork'} = 1;
	
	my $cmd = $task_template->{'cmd'};
	$task->{'cmd'} = undef;
	
	$task->{'cmd'}->{'description'} = $taskid ||  "description";   # since AWE does not accept string taskids, I move taskid into the description
	
	$task->{'taskid'} = $taskid; # will be replaced later by numeric taskid
	
	
	
	if (defined $h{'TROJAN'}) {
		push(@{$task_template->{'inputs'}}, '[TROJAN]');
		#print "use trojan XXXXXXXXXXXXXXXXX\n";
	}
	
	
	
	my $depends = {};
	
	my $inputs = {};
	foreach my $key_io (@{$task_template->{'inputs'}}) {
		
		my ($key) = $key_io =~ /^\[(.*)\]$/;
	
		unless (defined $key) {
			
			die "no input key found in: $key_io";
			next;
		}
		
		my $value = $h{$key};
		if (defined $value) {
			if (ref($value) eq 'ARRAY') {
				my ($source_type, $source, $filename) = @{$value};
				
				if ($source_type eq 'shock') {
					$inputs->{$filename}->{'node'} = $source;
					
				} elsif ($source_type eq 'task') {
					$inputs->{$filename}->{'origin'} = $source;
					$depends->{$source} = 1;
				} else {
					die "source_type $source_type unknown";
				}
				
				
				$inputs->{$filename}->{'host'} = $host;
				$cmd =~ s/\[$key\]/$filename/g;
			} else {
				die "array ref expected for key $key";
			}
			
			
			
		} else {
			die "input key \"$key\" not defined";
		}
	}
	$task->{'inputs'}=$inputs;
	
	
	my $outputs = {};
	
	if (!defined ($task_template->{'outputs'}) || @{$task_template->{'outputs'}} == 0) {
		print Dumper($task_template)."\n";
		die "no outputs found in task template for task \"$taskid\"";
	}
	
	foreach my $key_io (@{$task_template->{'outputs'}}) {
		print "key_io: $key_io\n";
		
		my ($key) = $key_io =~ /^\[(.*)\]$/;
		
		#replace variable if possible
		if (defined $key) {
		
			my $value = $h{$key};
			if (defined $value) {
				#$outputs->{$value}->{'host'} = $host;
				$cmd =~ s/\[$key\]/$value/g;
			} else {
				
				print Dumper($task_template);
				
				die "output key \"$key\" not defined";
			}
			$key_io = $value;
		}
		
		
			
		my $filename_base = basename($key_io);
		my $dir = dirname($key_io);
		
		unless ($dir eq '.') {
			$outputs->{$filename_base}->{'directory'} = $dir;
		}
		
		#die "key not defined in output";
		$outputs->{$filename_base}->{'host'} = $host;
		
		
		
		
		
	
	}
	$task->{'outputs'}=$outputs;
	
	$task->{'cmd'}->{'args'} = $cmd;
	
	
	my @depends_on = ();
	foreach my $dep (keys %$depends) {
		push(@depends_on, $dep);
	}
	if (@depends_on > 0 ) {
		$task->{'dependsOn'} = \@depends_on;
	}
	
	#print "task:\n";
	#print Dumper($task)."\n";
	#exit(0);
	
	return $task;
}



sub assignTasks {
	my ($self) = @_;
	
	
	
	my $task_specs = $self->{'tasks'};
	
	
	#replace variables in tasks
	for (my $i =0  ; $i < @{$task_specs} ; ++$i) {
		my $task_spec = $task_specs->[$i];
		
		#print "ref: ".ref($task)."\n";
		#print "keys: ".join(',',keys(%$tasks))."\n";
		#print Data::Dumper($task);
		
		print "task_spec:\n";
		print Dumper($task_spec);
		#my $trojan_file=$task->{'trojan_file'};
		
		

		### createTask ###
		my $newtask = createTask($self, %$task_spec);
		$self->{'data'}->{'tasks'}->[$i] = $newtask;
		
		#if (defined($task_spec->{'TROJAN'})) {
		#	$newtask->{'trojan_file'} = ${$task_spec->{'TROJAN'}}[2];
		#};
		
		#$task = $tasks->[$i];
		
		#print Data::Dumper($task);
		#exit(0);
		
		#my $trojan = $task->{'trojan'};
		
		#my $inputs = $task->{'inputs'};
		
	}
}


# search for [variable] inf workflow and replace with SHOCK information
sub _assignInput {
	my ($data, $task_specs, %h) = @_;
	
	my $tasks = $data->{'tasks'};
	for (my $i =0  ; $i < @{$tasks} ; ++$i) {
		my $task = $tasks->[$i];
		
		#my $task_spec = $self->{'tasks'}->[$i];
		my $task_spec = $task_specs->[$i];
		
		#my $trojan_file=$task->{'trojan_file'};
		my $trojan_file=undef;
		if (defined($task_spec->{'TROJAN'})) {
			$trojan_file = ${$task_spec->{'TROJAN'}}[2];
		}
		
		#print Dumper($task);
		my $inputs = $task->{'inputs'};
		
		foreach my $inputfile (keys(%{$inputs})) {
			#print "inputfile: $inputfile\n";
			my $input_obj = $inputs->{$inputfile};
			
			
			if (defined $input_obj->{'node'}) {
				
				# search for [variable]
				my ($variable) = $input_obj->{'node'} =~ /\[(.*)\]/;
				
				if (defined $variable) {
					my $file_obj = $h{$variable};
					if (defined($file_obj)) {
						unless (defined $file_obj->{'node'}) {
							die "node not defined for input $variable";
						}
						unless (defined $file_obj->{'shockhost'}) {
							die "shockhost not defined for input $variable";
						}
						$input_obj->{'node'} = $file_obj->{'node'};
						$input_obj->{'host'} = $file_obj->{'shockhost'};
					} else {
						die "no replacement for variable $variable found";
					}
				}
				
				
			}
		}
		
		#my $outputs = $task->{'outputs'};
		#foreach my $outputfile (keys(%{$outputs})) {
		#	my $output_obj = $outputs->{$outputfile};
			
			
		#}
		
		
		#print Dumper($task);
		#exit(0);
		#print "got: ".$task->{'cmd'}->{'args'}."\n";
		
		unless (defined $task->{'cmd'}) {
			die;
		}
		
		unless (defined $task->{'cmd'}->{'args'}) {
			die;
		}
		
		
		
		if (defined($trojan_file)) {
			#if ( defined($h{'TROJAN'}) ) {
			
			# modify AWE task to use trojan script
			
			
			
			$task->{'cmd'}->{'args'} = "\@".$trojan_file." ".$task->{'cmd'}->{'args'};
			$task->{'cmd'}->{'name'} = "perl";
			
			
		} else {
			# extract the executable from command
			
			my $executable;
			$task->{'cmd'}->{'args'} =~ s/^([\S]+)//;
			$executable=$1;
			$task->{'cmd'}->{'args'} =~ s/^[\s]*//;
			
			unless (defined $executable) {
				die "executable not found in ".$task->{'cmd'}->{'args'};
			}
			
			$task->{'cmd'}->{'name'} = $executable;
		}
	}
}


# this assigns input to internal data
sub assignInput {
	my ($self, %h) = @_;
	# $h contains named_input to shock node mapping
	my $data = $self->{'data'};
	
	_assignInput($data, $self->{'tasks'}, %h);
	
	
}

sub replace_taskids {
	my ($self) = @_;
	
	# $h contains named_input to shock node mapping
	my $tasks = $self->{'data'}->{'tasks'};
	my $taskid_num = {};
	my $taskid_count = 0;
	
	#replace taskids with strings of numbers !
	for (my $i =0  ; $i < @{$tasks} ; ++$i) {
		my $task = $tasks->[$i];
		my $taskid = $task->{'taskid'};
		
		unless (defined $taskid_num->{$taskid}) {
			$taskid_num->{$taskid} = $taskid_count;
			$taskid_count++;
		}
		$task->{'taskid'}= $taskid_num->{$taskid}.'';
		
		
		my $dependsOn = $task->{'dependsOn'};
		if (defined $dependsOn) {
			my $dependsOn_new = [];
			foreach my $dep_task (@$dependsOn) {
				unless (defined $taskid_num->{$dep_task}) {
					$taskid_num->{$dep_task} = $taskid_count;
					$taskid_count++;
				}
				
				push(@$dependsOn_new, $taskid_num->{$dep_task}.'');
			}
			if (@$dependsOn_new > 0) {
				$task->{'dependsOn'} = $dependsOn_new;
			}
		}
		
		my $inputs = $task->{'inputs'};
		
		
		foreach my $input (keys(%$inputs)) {
			
			if (defined($inputs->{$input}->{'origin'})) {
				my $origin = $inputs->{$input}->{'origin'};
				unless (defined $taskid_num->{$origin}) {
					$taskid_num->{$origin} = $taskid_count;
					$taskid_count++;
				}
				$inputs->{$input}->{'origin'} = $taskid_num->{$origin}.'';
			}
		}
		
		
		
	}
	
}

#returns clone
sub hash {
	my ($self) = @_;
	#return {%$self}

	return dclone($self->{'data'});
}

sub json {
	my ($self) = @_;
	my $job_hash = hash($self);
	
	my $json = JSON->new;
	my $job_json = $json->encode( $job_hash );
	return $job_json;
}


sub create {
	my ($self, %h) = @_;
	
	
	my $data_copy = hash($self);
	_assignInput($data_copy, $self->{'tasks'}, %h);
	
	return $data_copy;
}

############################################
#the trojan horse generator
# - creates log files (stdin, stderr)
# - ENV support
# - archives directories
# - scripts on VM do not need to be registered at AWE client
sub get_trojanhorse {
	my %h = @_;
	
	print "get_trojanhorse got: ".join(' ' , keys(%h))."\n";
	#exit(0);
	
	my $out_dirs = $h{'out_dirs'};
	my $resulttarfile = $h{'tar'};
	
	my $out_files = $h{'out_files'};
	
	
	# start of trojanhorse_script
	my $trojanhorse_script = <<'EOF';
	#!/usr/bin/env perl
	
	use strict;
	use warnings;
	
	use IO::Handle;
	
	open OUTPUT, '>', "trojan.stdout" or die $!;
	open ERROR,  '>', "trojan.stderr"  or die $!;
	
	STDOUT->fdopen( \*OUTPUT, 'w' ) or die $!;
	STDERR->fdopen( \*ERROR,  'w' ) or die $!;
	
	sub systemp {
		print "cmd: ".join(' ', @_)."\n";
		return system(@_);
	}
	
	
	eval {
		print "hello trojan world\n";
		
		my $line = join(' ',@ARGV)."\n";
		print "got: ".$line;
		
		
		my $check = join('|', keys %ENV);
		$line =~ s/\$($check)/$ENV{$1}/g;
		
		systemp($line)==0 or die;
		
	};
	if ($@) {
		print "well.. there was an error, but I catched it.. ;-)\n";
	}
	###TAR###
	
	###OUTFILES###
	
	close(OUTPUT);
	close(ERROR);
	select STDOUT; # back to normal
	
EOF
	# end of trojanhorse_script
	
	if (defined $out_dirs && @$out_dirs > 0) {
		my $outdirsstr = join(' ', @$out_dirs);
		
		# start of tarcmd
		my $tarcmd = <<EOF;
		systemp("tar --ignore-failed-read -cf $resulttarfile $outdirsstr trojan.stdout trojan.stderr")==0 or die;
EOF
		# end of tarcmd
		
		$trojanhorse_script =~ s/\#\#\#TAR\#\#\#/$tarcmd/g;
	}
	
	if (defined $out_files && @$out_files > 0) {
		my $out_files_str = join(' ', @$out_files);
		
		# start of tarcmd
		my $cpcmd = <<EOF;
		systemp("cp $out_files_str .")==0 or die;
EOF
		# end of tarcmd
		
		$trojanhorse_script =~ s/\#\#\#OUTFILES\#\#\#/$cpcmd/g;
	}
	
	
	return $trojanhorse_script;
}
# end of trojan generator
############################################



sub parse_command {
	
	my $command_str = shift(@_);
	
	
	
	
	print "COMMAND_a: ".$command_str."\n";
	my @COMMAND = split(/\s/, $command_str); # TODO need better way for this!?
	
	#print "split: ". join(',', @COMMAND)."\n";
	#exit(0);
	
	
	my @input_files_local=();
	my @output_files=();
	my @output_directories=();
	
	
	
	
	for (my $i=0; $i <@COMMAND ; ++$i) {
		
		if ($COMMAND[$i] =~ /^@@@/) {
			#print "at $ARGV[$i]\n";
			my $output_directory = substr($COMMAND[$i], 3);
			print "output_directory: $output_directory\n";
			push (@output_directories, $output_directory);
			$COMMAND[$i] = $output_directory; # need to encode info about directory in trojan script
		} elsif ($COMMAND[$i] =~ /^@@/) {
			#print "at $ARGV[$i]\n";
			my $output_file = substr($COMMAND[$i], 2);
			print "output_file: $output_file\n";
			if (-e $output_file) {
				print STDERR "error: output_file \"$output_file\" already exists\n";
				exit(1);
			}
			
			
			my $id = @output_files;
			push(@output_files, $output_file);
			$COMMAND[$i] = $output_file;
			#$COMMAND[$i] = '[OUTPUT'.$id.']';
		} elsif ($COMMAND[$i] =~ /^@/) {
			#print "at $ARGV[$i]\n";
			my $input_file = substr($COMMAND[$i], 1);
			print "input_file: $input_file\n";
			
			unless (-e $input_file) {
				print STDERR "error: file $input_file not found\n";
				exit(1);
			}
			
			my $id = @input_files_local;
			push(@input_files_local, $input_file);
			$COMMAND[$i] = '@[INPUT'.$id.']';
			#$COMMAND[$i] = '@'.basename($input_file);
			
			
		}
		
	}
	
	my $cmd = join(' ',@COMMAND);
	print "COMMAND_b: ".$cmd."\n";
	
	
	my $resulttarfile = 'x.tar';
	
	if (@output_directories > 0) {
		$resulttarfile = $output_directories[0];
		$resulttarfile =~ s/\///g;
		$resulttarfile.='.tar';
		
		if (-e $resulttarfile) {
			print STDERR $resulttarfile." already exists\n";
			exit(1);
		}
		
	}
	
	return (\@input_files_local , \@output_files, \@output_directories, $cmd);
}




sub generateAndSubmitSimpleAWEJob {
	my %h = @_;
	
	my $command = $h{'cmd'}; # example
	
	my $clientgroup = $h{'clientgroup'} || die "no clientgroup defined";
	my $awe_user = "awe_user";
	
	
	my $awe = $h{'awe'};
	my $shock = $h{'shock'};
	
	
	#parse input/output
	
	my ($input_files_local, $output_files, $output_directories, $command_parsed) = &parse_command($command);
	if (defined $h{'output_files'} ) {
		my @of = split(',', $h{'output_files'});
		push(@{$output_files}, @of);
	}
	
	
	
	### create task template ###
	my $task_template={};
	$task_template->{'cmd'} = $command_parsed;
	for (my $i=0 ; $i < @{$input_files_local} ; ++$i ) {
		push(@{$task_template->{'inputs'}}, '[INPUT'.$i.']' );
	}
	
	for (my $i=0 ; $i < @{$output_files} ; ++$i ) {
		my $outputfile = $output_files->[$i];
		
		
		#my ($outputfilename, $outputfilepath) = fileparse($fullname);
		
		push(@{$task_template->{'outputs'}}, $outputfile);
	}
	#if (defined $h{'output_files'} ) {
	#	my @of = split(',', $h{'output_files'});
	#	foreach my $file (@of) {
	#		push(@{$task_template->{'outputs'}}, basename($file) );
	#	}
	#
	#	$task_template->{'trojan'}->{'out_files'}=\@of;
	#}
	
	print "generated template:\n";
	print Dumper($task_template);
	#exit(0);
	
	### create task (using the above generated template) ###
	my $task = {
		"task_id" => "single_task",
		"task_template" => "template",
#		"TROJAN" => ["shock", "[TROJAN1]", "trojan1.pl"]
	};
	
	
	my @inputs=();
	for (my $i=0 ; $i < @{$input_files_local} ; ++$i ) {
		my $inputfile = $input_files_local->[$i];
		$task->{'INPUT'.$i} = ["shock", "[INPUT".$i."]", $inputfile];
		push(@inputs, 'INPUT'.$i);
	}
	#$task->{'inputs'} = \@inputs;
	
	my @outputs=();
	for (my $i=0 ; $i < @{$output_files} ; ++$i ) {
		my $outputfile = $output_files->[$i];
		$task->{'OUTPUT'.$i} = $outputfile;

		push(@outputs, $outputfile);
		#print "push: ".basename($outputfile)."\n";
	}
		
	print "generated task (without input):\n";
	print Dumper($task);
	
	#exit(0);
	
	
	#$task->{'outputs'} = \@outputs;
	
	
	#my $task_tmpls={};
	#$task_tmpls->{'template'} = $task_template;
	
	
	#print "task:\n";
	#print Dumper($task);
	
	my $awe_qiime_job = AWE::Job->new(
	'info' => {
		"pipeline"=> "simple-autogen",
		"name"=> "simple-autogen-name",
		"project"=> "simple-autogen-prj",
		"user"=> $awe_user,
		"clientgroups"=> $clientgroup,
		"noretry"=> JSON::true
	},
	'shockhost' => $shock->{'shock_url'},
	'task_templates' => {'template' => $task_template}, # only one template in hash
	'tasks' => [$task]
	);
	
	
	
	### define job input ###
	my $job_input = {};
	
	#if (defined $h{'output_files'} ) {
	#	my @of = split(',', $h{'output_files'});
	#	foreach my $file (@of) {
	#		push(@outputs, basename($file));
	#		print "push: ".basename($file)."\n";
	#	}
		#$job_input->{'TROJAN1'}->{'data'} = AWE::Job::get_trojanhorse("out_files" => \@of) ;
	#} else  {
		#$job_input->{'TROJAN1'}->{'data'} = AWE::Job::get_trojanhorse() ;
	#}
	#$job_input->{'TROJAN1'}->{'node'}= "fake_shock_node_trojan1";
	#$job_input->{'TROJAN1'}->{'shockhost'}= "fake_host";
	
	
	
	# local files to be uploaded
	for (my $i=0 ; $i < @{$input_files_local} ; ++$i ) {
		my $inputfile = $input_files_local->[$i];
		$job_input->{'INPUT'.$i}->{'file'} = $inputfile;
		#$job_input->{'INPUT'.$i}->{'node'} = "fake_shock_node".$i;
		#$job_input->{'INPUT'.$i}->{'shockhost'}= "fake_host";
	}
	
	
	
	
	
	#$job_input->{'INPUT-PARAMETER'}->{'file'} = './otu_picking_params_97.txt';   	  # local file to be uploaded
	
	#print Dumper($job_input);
	
	
	#upload job input files
	$shock->upload_temporary_files($job_input);
	print "all temporary files uploaded.\n";
	
	# create job with the input defined above
	my $workflow = $awe_qiime_job->create(%$job_input);
	
	#exit(0);
	
	#overwrite jobname:
	#$workflow->{'info'}->{'name'} = $sample;
	
	my $json = JSON->new;
	print "AWE job ready for submission:\n".$json->pretty->encode( $workflow )."\n";
#exit(0);
	print "submit job to AWE server...\n";
	my $submission_result = $awe->submit_job('json_data' => $json->encode($workflow));
	
	print "result from AWE server:\n".$json->pretty->encode( $submission_result )."\n";
	
	return $submission_result->{'data'}->{'id'};
}


# return 0 if all jobs are completed
sub check_jobs {
	
	my %h = @_;
	
	my $awe = $h{'awe'};
	my $jobs= $h{'jobs'};
	my $clientgroup = $h{'clientgroup'};
	
	
	unless (defined $awe) {
		die;
	}
	
	
	my $job_hash={};
	foreach my $job (@$jobs) {
		$job_hash->{$job}=1;
	}
	
	
	
	
	
	my $all_jobs = $awe->getJobQueue('info.clientgroups' => $clientgroup);
	
	
	my $all_jobs_hash = {};
	
	foreach my $job_object (@{$all_jobs->{data}}) {
		my $job = $job_object->{'id'};
		$all_jobs_hash->{$job} = $job_object;
	}
	
	
	foreach my $job (@$jobs) {
		my $job_object = $all_jobs_hash->{$job};
		
		unless (defined $job_object) {
			return 1;
		}
		
		unless ($job_object->{'state'} eq "completed") { # TODO need to detect fail state !!!
			return 1;
		}
		
	}
	
	
	return 0;
	
}

sub get_jobs {
	
	my %h = @_;
	
	my $awe = $h{'awe'};
	my $jobs= $h{'jobs'};
	my $clientgroup = $h{'clientgroup'};
	my $properties = $h{'properties'};
	
	unless (defined $awe) {
		die;
	}
		
	
	my $job_hash={};
	foreach my $job (@$jobs) {
		$job_hash->{$job}=1;
	}
	
	
	
	
	
	my $all_jobs = $awe->getJobQueue('info.clientgroups' => $clientgroup);
	
	#print Dumper($all_jobs);
	
	
	
	
	
	
	# get list of job objects
	my @requested_jobs = ();
	
	foreach my $job_object (@{$all_jobs->{data}}) {
		
		my $job = $job_object->{'id'};
		
		unless (defined($job_hash->{$job})) {
			next;
		}
		
		my $skip = 0;
		foreach my $p (keys(%$properties)) {
			my $pval = $properties->{$p};
			if ($job_object->{$p} ne $pval) {
				$skip =1 ;
				last;
			}
			if ($skip == 1) {
				last;
			}
			
		}
		
		if ($skip == 1) {
			next;
		}
		
		#unless ($job_object->{'state'} eq "completed") {
		#	print STDERR "warning: job $job not yet completed\n";
		#	next;
		#}
		
		push(@requested_jobs, $job_object);
	}

	print 'get_jobs returns: '.@requested_jobs."\n";
	return  @requested_jobs;
}


sub download_jobs {
	
	my %h = @_;
	
	my $awe = $h{'awe'};
	my $shock= $h{'shock'};
	my $jobs= $h{'jobs'};
	my $clientgroup = $h{'clientgroup'};
	my $use_download_dir = $h{'use_download_dir'};
	my $only_last_task = $h{'only_last_task'};
	
	
	my @requested_jobs = get_jobs(@_, 'properties' => {'state' => 'completed'});
	
	my $jobs_to_process = @requested_jobs;
	print "jobs_to_process: $jobs_to_process\n";
	
	# download results, delete results, delete job
	my $job_deletion_ok= 1;
	foreach my $job_object (@requested_jobs) {
		
		my $job_id = $job_object->{'id'};
		
		
		#print "completed job $job\n";
		
		print Dumper($job_object)."\n";
		
		download_output_job_nodes($job_object, $shock, 'only_last_task' => $only_last_task, 'use_download_dir' => $use_download_dir);
		
		
		$jobs_to_process--;
		
		
	}
	
	if ($jobs_to_process != 0 ) {
		die "not all jobs processed";
	}
	
	
	return 0;
}


#deletes all "temporary" shock nodes of a given list of AWE job IDs
sub delete_jobs {
	
	my %h = @_;
	
	my $awe = $h{'awe'};
	my $shock= $h{'shock'};
	my $jobs= $h{'jobs'};
	my $clientgroup = $h{'clientgroup'};
	
	#'properties' => {'state' => 'completed'}
	my @requested_jobs = get_jobs(@_);
	#my @requested_jobs = @$jobs;
	
	
	my $jobs_to_process = @requested_jobs;
	print "jobs_to_process: $jobs_to_process\n";
	
	# download results, delete results, delete job
	my $job_deletion_ok= 1;
	foreach my $job_object (@requested_jobs) {
		
		my $job_id = $job_object->{'id'};
		
		#print "got jobid: $job_id\n";
		#print "completed job $job\n";
		
		print Dumper($job_object)."\n";
		
				
		my $node_delete_status = delete_output_job_nodes($job_object, $shock);
		
		if (defined $node_delete_status) {
			print "deleting job ".$job_id."\n";
			my $dd = $awe->deleteJob($job_id);
			print Dumper($dd);
		} else {
			$job_deletion_ok = 0;
		}
		$jobs_to_process--;
		
		
	}
	
	if ($jobs_to_process != 0 ) {
		die "not all jobs processed";
	}
	
	if ($job_deletion_ok == 1) {
		return 1;
	}
	
	return 0;
}

sub wait_and_download_job_results {
	my %h = @_;
	
	my $awe = $h{'awe'};
	my $shock= $h{'shock'};
	my $jobs= $h{'jobs'};
	my $clientgroup = $h{'clientgroup'};
	
	
	unless (defined $awe) {
		die;
	}
	unless (defined $shock) {
		die;
	}

	
	
	my $jobs_to_download = {};
	
	foreach my $job_id (@$jobs) {
		$jobs_to_download->{$job_id} = 1;
	}
	
	
	my $got_all=0;
	while ($got_all==0) {
		sleep(5);
		
		$got_all=1;
		foreach my $job_id (@$jobs) {
			my $waiting = $jobs_to_download->{$job_id};
			
			if ($waiting == 1) {
				$got_all=0;
				
				my $jobstatus_hash;
				eval {
					$jobstatus_hash = $awe->getJobStatus($job_id);
				};
				if ($@) {
					print "error: getJobStatus $job_id\n";
					exit(1);
				}
				#print $json->pretty->encode( $jobstatus_hash )."\n";
				my $state = $jobstatus_hash->{data}->{state};
				print "state: $state\n";
				if ($state ne 'completed') {
					next;
				}
				print "job $job_id ready, download results\n";
				
				download_jobs('awe' => $awe, 'shock' => $shock, 'jobs' => [$job_id], 'clientgroup' => $clientgroup);
				
				$jobs_to_download->{$job_id} = 0;
				
			}
			
		}
		
		
		
	}
	print "finished.\n";
}


sub download_output_job_nodes {
	my ($job_hash, $shock, %h) = @_;
	
	
	
	
	my $download_dir = ".";
	
	if (defined $h{'use_download_dir'} && $h{'use_download_dir'} == 1) {
		my $job_name = $job_hash->{'info'}->{'name'} || die;
		$download_dir = $job_name;
		
		if (-d $download_dir) {
			die "download_dir \"$download_dir\" already exists\n";
		}
		
		system("mkdir -p ".$download_dir) == 0 or die;
		print STDERR "created output directory \"$download_dir\".\n";
		
	}
	
	
	
	
	my $download_output_nodes = get_awe_output_nodes($job_hash, %h);
	
	my $download_success = download_ouput_from_shock($shock, $download_output_nodes, $download_dir);
	
	
	if ($download_success == 0 ) {
		die "download failed";
	}
	
}

sub get_awe_output_nodes {
	my ($job_hash, %h) = @_;
	
	
	my $output_nodes = {};
	
	
	my @tasks;
	
	if (defined $h{'only_last_task'} && $h{'only_last_task'}==1) {
		@tasks = ($job_hash->{tasks}->[-1]); #TODO this is last task in json, not last by dependency !
	} else {
		@tasks = @{$job_hash->{tasks}};
	}
	
	foreach my $task (@tasks) {
		
		if (defined $task->{outputs}) {
			my $outputs = $task->{outputs};
			
			foreach my $resultfilename (keys(%$outputs)) {
				
				if (defined $output_nodes->{$resultfilename}) {
					die "error: output filename not unique ($resultfilename)";
				}
				
				$output_nodes->{$resultfilename} = $outputs->{$resultfilename};
			}
			
			
		}
	}
	#print Dumper($output_nodes);
	#exit(0);
	return $output_nodes;
}

sub download_ouput_from_shock{
	my ($shock, $output_nodes, $download_dir, %h) = @_;
	
	my $download_success = 1 ;
	print Dumper($output_nodes);
	
	foreach my $resultfilename (keys(%$output_nodes)) {
		print "resultfilename: $resultfilename\n";
		
		my $download_name = $resultfilename;
		if (defined $download_dir ) {
			$download_name = $download_dir.'/'.$resultfilename;
		}
		
		if (-e $download_name) {
			print "\"$download_name\" already exists, refuse to overwrite...\n";
			exit(1);
		}
		
		my $result_obj = $output_nodes->{$resultfilename};
		unless (defined $result_obj) {
			die;
		}
		unless (ref($result_obj) eq 'HASH') {
			die;
		}
		
		
		
		
		my $result_node = $result_obj->{'node'};
		unless (defined $result_node) {
			die;
		}
		#my $result_size =  $result_obj->{size};
		
		#print Dumper($result_obj);
		
		
		
		if (defined $result_node) {
			#push(@temporary_shocknodes, $result_node);
			print "downloading $resultfilename...\n";
			$shock->download_to_path($result_node, $download_name);
			
		} else {
			print Dumper($result_obj);
			#exit(0);
			
			#print $json->pretty->encode( $jobstatus_hash )."\n";
			print STDERR "warning: no result found\n";
			$download_success=0;
			die;
		}
		
		
	}
	return $download_success;
}

sub delete_shock_nodes{
	my ($shock, $output_nodes) = @_;
	
	
	
	
	my $delete_ok = 1;
	foreach my $resultfilename (keys %$output_nodes) {
		
		my $result_obj = $output_nodes->{$resultfilename};
		my $node_to_be_deleted = $result_obj->{node};
		#my $result_size =  $result_obj->{size};
		
		if (defined $node_to_be_deleted) {
			
			# delete
			print "try to delete shock node $node_to_be_deleted\n";
			
			my $nodeinfo = $shock->get_node($node_to_be_deleted);
			
			if (defined $nodeinfo) {
				print Dumper($nodeinfo);
				
				my $deleteshock = $shock->delete_node($node_to_be_deleted);
								
				unless (defined $deleteshock && defined $deleteshock->{'status'} && $deleteshock->{'status'}==200) {
					print "error deleting $node_to_be_deleted\n";
					$delete_ok = 0;
				} else {
					print "deleted $node_to_be_deleted\n"
				}
			} else {
				print "warning: cannot delete node, node \"$node_to_be_deleted\" not found\n";
				next;
			}
			
		} else {
			#print $json->pretty->encode( $jobstatus_hash )."\n";
			print STDERR "warning: no result found\n";
			$delete_ok = 0;
		}
	}
	return $delete_ok;
}




#deletes all "temporary" shock nodes of an AWE job object
sub delete_output_job_nodes {
	my ($job_hash, $shock) = @_;
	
	
	my $all_output_nodes = get_awe_output_nodes($job_hash);
	
	
	
	### delete output shock nodes ####
	my $delete_ok = delete_shock_nodes($shock, $all_output_nodes);
	
	
	
	if ($delete_ok == 0) {
		return undef;
	} else {
		return 1;
	}
}


1;