#!/usr/bin/env perl
# =============================================================================
# discord-mcp — MCP server for Discord webhooks
#
# Repository: github.com/kirill-scherba/discord-mcp
#
# Features:
#   - discord_send — simple text message
#   - discord_send_embed — rich embed with color, title, fields
#   - discord_webhook_info — validate webhook connectivity
#   - DISCORD_WEBHOOK_URL from environment
#   - JSON-RPC 2.0 over stdin/stdout (MCP protocol)
# =============================================================================

use strict;
use warnings;
use utf8;
use JSON;
use POSIX qw(strftime);

use English '-no_match_vars';

# UTF-8 encoding
binmode(STDIN,  ":utf8");
binmode(STDOUT, ":utf8");
binmode(STDERR, ":utf8");

# Configuration
my $WEBHOOK_URL = $ENV{DISCORD_WEBHOOK_URL} // '';
log_message("INFO", "WEBHOOK_URL " . ($WEBHOOK_URL ? 'found' : 'NOT found'));

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
sub log_message {
    my ($level, $message) = @_;
    my $timestamp = strftime("%Y-%m-%d %H:%M:%S", localtime);
    print STDERR "[$timestamp] [$level] $message\n";
    STDERR->flush();
}

# ---------------------------------------------------------------------------
# JSON-RPC helpers
# ---------------------------------------------------------------------------
my $json = JSON->new->allow_nonref;

sub respond {
    my ($id, $result) = @_;
    my $response = { jsonrpc => "2.0", id => $id, result => $result };
    print $json->encode($response) . "\n";
    STDOUT->flush();
}

sub respond_error {
    my ($id, $code, $message) = @_;
    my $response = { jsonrpc => "2.0", id => $id, error => { code => $code, message => $message } };
    print $json->encode($response) . "\n";
    STDOUT->flush();
}

# ---------------------------------------------------------------------------
# Discord webhook helpers
# ---------------------------------------------------------------------------
sub _discord_send {
    my ($payload) = @_;
    die "DISCORD_WEBHOOK_URL not set" unless $WEBHOOK_URL;

    my $body = $json->encode($payload);
    my $tmp  = "/tmp/_discord_body_$$.json";
    open(my $fh, '>', $tmp) or die "Cannot write temp file: $!";
    print $fh $body;
    close $fh;

    log_message("DEBUG", "Sending to Discord webhook");
    my $result = `curl -s -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data-binary \@'$tmp' --connect-timeout 10 --max-time 15 '$WEBHOOK_URL' 2>/dev/null`;
    unlink $tmp if -f $tmp;

    my $http_code = '';
    if (length($result) >= 3) {
        $http_code = substr($result, -3, 3);
        $result = substr($result, 0, -3);
    }
    $http_code =~ s/\s+//g;

    if ($http_code =~ /^2/) {
        return { sent => 1, http_code => $http_code + 0 };
    }
    die "Discord webhook error: HTTP $http_code — $result";
}

# ---------------------------------------------------------------------------
# Tool: discord_send — send a simple text message
# ---------------------------------------------------------------------------
sub tool_discord_send {
    my ($args) = @_;
    my $message = $args->{message} or die "Missing required: message";
    my $payload = { content => $message };
    return _discord_send($payload);
}

# ---------------------------------------------------------------------------
# Tool: discord_send_embed — send a rich embed
# ---------------------------------------------------------------------------
sub tool_discord_send_embed {
    my ($args) = @_;
    my $title       = $args->{title}       // '';
    my $description = $args->{description} // '';
    my $color       = $args->{color}       // 5793266;  # Discord blurple
    my $fields      = $args->{fields}      // undef;    # array of {name, value, inline}

    my $embed = {
        title       => $title,
        description => $description,
        color       => $color + 0,
        timestamp   => strftime("%Y-%m-%dT%H:%M:%S.000Z", gmtime),
    };
    $embed->{fields} = $fields if $fields && ref $fields eq 'ARRAY' && @$fields;

    my $payload = { embeds => [$embed] };
    return _discord_send($payload);
}

# ---------------------------------------------------------------------------
# Tool: discord_webhook_info — validate webhook connectivity
# ---------------------------------------------------------------------------
sub tool_discord_webhook_info {
    die "DISCORD_WEBHOOK_URL not set" unless $WEBHOOK_URL;
    my $result = `curl -s -w '%{http_code}' -X GET --connect-timeout 10 --max-time 15 '$WEBHOOK_URL' 2>/dev/null`;
    my $http_code = '';
    if (length($result) >= 3) {
        $http_code = substr($result, -3, 3);
        $result = substr($result, 0, -3);
    }
    $http_code =~ s/\s+//g;

    my $data = eval { $json->decode($result) };
    if ($http_code eq '200' && $data) {
        return {
            name    => $data->{name} // '',
            channel => $data->{channel_id} // '',
            guild   => $data->{guild_id} // '',
            type    => $data->{type} // 0,
        };
    }
    die "Discord webhook check failed: HTTP $http_code";
}

# ============================================================================
# Tool handlers table
# ============================================================================
my %TOOLS = (
    discord_send => {
        name        => 'discord_send',
        description => 'Send a simple text message to Discord via webhook.',
        inputSchema => {
            type => 'object',
            properties => {
                message => { type => 'string', description => 'Message text to send' },
            },
            required => ['message'],
        },
        handler => \&tool_discord_send,
    },
    discord_send_embed => {
        name        => 'discord_send_embed',
        description => 'Send a rich embed message to Discord via webhook. Supports title, description, color, and fields.',
        inputSchema => {
            type => 'object',
            properties => {
                title       => { type => 'string', description => 'Embed title' },
                description => { type => 'string', description => 'Embed description text' },
                color       => { type => 'integer', description => 'Embed color as decimal (default: 5793266 blurple)' },
                fields      => {
                    type  => 'array',
                    description => 'Array of field objects with name, value, inline',
                    items => {
                        type => 'object',
                        properties => {
                            name   => { type => 'string' },
                            value  => { type => 'string' },
                            inline => { type => 'boolean' },
                        },
                        required => ['name', 'value'],
                    },
                },
            },
            required => [],
        },
        handler => \&tool_discord_send_embed,
    },
    discord_webhook_info => {
        name        => 'discord_webhook_info',
        description => 'Check Discord webhook connectivity and return info.',
        inputSchema => { type => 'object', properties => {}, required => [] },
        handler     => \&tool_discord_webhook_info,
    },
);

# ============================================================================
# MCP server main loop
# ============================================================================
my $initialized = 0;

while (my $line = <STDIN>) {
    chomp $line;
    next unless $line;

    my $msg = eval { $json->decode($line) };
    if ($@) {
        log_message("ERROR", "Failed to parse JSON: $@");
        next;
    }

    my $method = $msg->{method} // '';
    my $id     = $msg->{id};

    log_message("DEBUG", "Received: $method");

    if ($method eq 'initialize') {
        respond($id, {
            protocolVersion => '2024-11-05',
            capabilities    => { tools => {} },
            serverInfo      => { name => 'discord-mcp', version => '1.0.0' },
        });
        $initialized = 1;
        next;
    }

    unless ($initialized) {
        respond_error($id, -32000, 'Not initialized');
        next;
    }

    if ($method eq 'tools/list') {
        my @list = map { {
            name        => $_->{name},
            description => $_->{description},
            inputSchema => $_->{inputSchema},
        } } values %TOOLS;
        respond($id, { tools => \@list });
        next;
    }

    if ($method eq 'tools/call') {
        my $tool_name = $msg->{params}{name} // '';
        my $args      = $msg->{params}{arguments} // {};

        my $tool = $TOOLS{$tool_name};
        unless ($tool) {
            respond_error($id, -32601, "Tool not found: $tool_name");
            next;
        }

        eval {
            my $result = $tool->{handler}($args);
            respond($id, $result);
        };
        if ($@) {
            log_message("ERROR", "Tool $tool_name failed: $@");
            respond_error($id, -32603, "Internal error: $@");
        }
        next;
    }

    if ($method eq 'notifications/initialized') {
        next;
    }

    respond_error($id, -32601, "Method not found: $method");
}

log_message("INFO", "Server shutting down");
