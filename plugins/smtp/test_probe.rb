# frozen_string_literal: true

# Tests for the SMTP prober. Run: ruby test_probe.rb
# A fake SMTP server on localhost stands in for the real thing, so nothing leaves
# the machine. The prober is exercised the way fogping does: as a program that takes
# JSON on stdin and prints JSON.

require 'json'
require 'minitest/autorun'
require 'open3'
require 'openssl'
require 'socket'

PROBE = File.expand_path('probe', __dir__)

# Serves one script of replies per connection, then hangs up.
class FakeSMTP
  attr_reader :port, :seen

  def initialize(greeting: "220 fake ESMTP\r\n", ehlo: "250-fake\r\n250 8BITMIME\r\n")
    @seen = []
    @server = TCPServer.new('127.0.0.1', 0)
    @port = @server.addr[1]
    @thread = Thread.new do
      loop do
        c = @server.accept
        Thread.new(c) do |s|
          s.write(greeting) unless greeting.nil?
          while (line = s.gets)
            @seen << line.strip
            case line
            when /\AEHLO/ then s.write(ehlo)
            when /\AQUIT/ then s.write("221 bye\r\n") && break
            end
          end
          s.close
        end
      end
    end
  end

  def close
    @thread.kill
    @server.close
  end
end

def run_probe(port, options = {}, host: '127.0.0.1', packets: 2, timeout: 1000)
  req = { protocol: 'fogping-probe/1', type: 'smtp', host: host, port: port,
          packets: packets, gap_ms: 1, timeout_ms: timeout, options: options }
  out, = Open3.capture2(PROBE, stdin_data: JSON.generate(req))
  JSON.parse(out)
end

class ProbeTest < Minitest::Test
  def with_server(**kw)
    srv = FakeSMTP.new(**kw)
    yield srv
  ensure
    srv&.close
  end

  def test_healthy_server_at_both_stages
    with_server do |srv|
      %w[banner ehlo].each do |stage|
        a = run_probe(srv.port, { 'stage' => stage })
        assert_equal [2, 2, 2], [a['sent'], a['received'], a['ms'].length], stage
      end
      assert_includes srv.seen, 'EHLO fogping.invalid'
      assert_includes srv.seen, 'QUIT'
    end
  end

  def test_banner_stage_never_sends_ehlo
    with_server do |srv|
      run_probe(srv.port, { 'stage' => 'banner' })
      refute(srv.seen.any? { |l| l.start_with?('EHLO') })
    end
  end

  def test_unhealthy_replies_are_loss
    with_server(greeting: "421 go away\r\n") { |s| assert_equal 0, run_probe(s.port)['received'] }
    with_server(greeting: "hello there\r\n") { |s| assert_equal 0, run_probe(s.port)['received'] }
    with_server(ehlo: "500 what\r\n") { |s| assert_equal 0, run_probe(s.port)['received'] }
    with_server(greeting: "#{'x' * 5000}\r\n") { |s| assert_equal 0, run_probe(s.port)['received'] }
  end

  def test_silent_server_times_out_as_loss
    with_server(greeting: nil) do |srv|
      t0 = Time.now
      a = run_probe(srv.port, {}, timeout: 200)
      assert_equal [0, []], [a['received'], a['ms']]
      assert_operator Time.now - t0, :<, 3
    end
  end

  def test_nothing_listening_is_loss_not_an_error
    s = TCPServer.new('127.0.0.1', 0)
    port = s.addr[1]
    s.close
    assert_equal 0, run_probe(port)['received']
  end

  def test_multiline_greeting_is_read_to_the_end
    with_server(greeting: "220-one\r\n220-two\r\n220 three\r\n") do |srv|
      assert_equal 2, run_probe(srv.port)['received']
    end
  end

  def test_own_failures_are_errors_with_no_measurement
    [
      run_probe(25, { 'helo_name' => "x\r\nMAIL FROM:<a@b>" }), # command injection attempt
      run_probe(25, {}, host: 'a b'),
      run_probe(0)
    ].each do |a|
      assert a.key?('error'), a.inspect
      refute a.key?('sent')
    end
    out, = Open3.capture2(PROBE, stdin_data: 'not json')
    assert JSON.parse(out).key?('error')
  end
end

# TLS is the part most worth testing: a prober that quietly accepts any certificate
# would report a healthy mail server that real clients would refuse.
class TLSTest < Minitest::Test
  def selfsigned_server
    key = OpenSSL::PKey::RSA.new(2048)
    cert = OpenSSL::X509::Certificate.new
    cert.version = 2
    cert.serial = 1
    cert.subject = cert.issuer = OpenSSL::X509::Name.parse('/CN=localhost')
    cert.public_key = key.public_key
    cert.not_before = Time.now - 60
    cert.not_after = Time.now + 3600
    cert.sign(key, OpenSSL::Digest.new('SHA256'))
    ctx = OpenSSL::SSL::SSLContext.new
    ctx.cert = cert
    ctx.key = key
    tcp = TCPServer.new('127.0.0.1', 0)
    server = OpenSSL::SSL::SSLServer.new(tcp, ctx)
    thread = Thread.new do
      loop do
        c = server.accept
        c.write("220 tls fake\r\n")
        c.close
      rescue StandardError
        nil
      end
    end
    [tcp.addr[1], thread, tcp]
  end

  def test_an_untrusted_certificate_is_loss
    port, thread, tcp = selfsigned_server
    a = run_probe(port, { 'security' => 'tls', 'stage' => 'banner' }, host: 'localhost')
    assert_equal [2, 0], [a['sent'], a['received']]
  ensure
    thread&.kill
    tcp&.close
  end
end
