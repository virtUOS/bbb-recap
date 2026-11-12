# frozen_string_literal: true

# Contract tests for the post_archive hook (docs/reference/bbb-exporter.md §3, test
# CT-EXP-01 in §10). Black-box: each test runs the hook as BBB does,
# `ruby <hook> -m <record_id>`, against the fixture raw dirs in testdata/ and
# compares the result with testdata/meetings/F-NN/expected/hook.json.
#
# The hook reads its settings from /etc/recap-exporter/exporter.env (§3.1).
# These tests write their own env file and pass its path in
# RECAP_EXPORTER_ENV_FILE, which names another config file path (§8.1).
#
# Without the hook file every test skips and names it.

require 'minitest/autorun'
require 'fileutils'
require 'json'
require 'open3'
require 'rbconfig'
require 'time'
require 'tmpdir'

class RecapHookContractTest < Minitest::Test
  ROOT = File.expand_path('../../..', __dir__)
  HOOK_REL = 'hook/post_archive/recap_export.rb'
  HOOK = File.join(ROOT, HOOK_REL)
  MEETINGS = File.join(ROOT, 'testdata/meetings')
  ENV_FILE_VAR = 'RECAP_EXPORTER_ENV_FILE'

  # Values from the fixtures that must never reach the hook's log (§3.1: the
  # log carries record_id and decision only).
  CANARIES = [
    'fixture-meeting-', 'Fixture Widget Meeting', 'Testperson', 'meeting-summary',
    'docs-main', 'PILOS', 'w_fix'
  ].freeze

  SPOOL_KEYS = %w[hook_version hooked_at record_id spool_version].freeze
  RFC3339_UTC = /\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z\z/

  # Fixtures with their own raw dir and a hook.json (F-07 and F-09 reuse F-01's).
  def self.fixtures
    Dir.children(MEETINGS).sort.select do |id|
      File.directory?(File.join(MEETINGS, id, 'raw')) &&
        File.file?(File.join(MEETINGS, id, 'expected/hook.json'))
    end
  end

  def setup
    skip "skipped: hook not present: #{HOOK_REL}" unless File.file?(HOOK)
    @sandbox = Dir.mktmpdir('recap-hook-')
    @spool = File.join(@sandbox, 'spool')
    @cwd = File.join(@sandbox, 'cwd')
    FileUtils.mkdir_p([@spool, @cwd])
  end

  def teardown
    FileUtils.rm_rf(@sandbox) if @sandbox
  end

  # Decision table §3.3 and spool entry §3.4, one test per fixture.
  fixtures.each do |id|
    define_method("test_decision_table_#{id.tr('-', '_')}") do
      expected = JSON.parse(File.read(File.join(MEETINGS, id, 'expected/hook.json')))
      raw_dir = File.join(MEETINGS, id, 'raw')
      record_id = only_child(raw_dir)
      before = tree_snapshot(raw_dir)

      status, output = run_hook(record_id, raw_dir: raw_dir)

      assert_equal expected['exit_code'], status.exitstatus, "#{id}: exit code\n#{output}"
      assert_spool(expected['spool_entry'], record_id, output)
      assert_log(expected['decision'], record_id, output)
      assert_equal before, tree_snapshot(raw_dir), "#{id}: the hook changed the raw dir"
      assert_writes_only_spool
    end
  end

  # §3.4: an existing entry for the record is left alone (`bbb-record --rebuild`).
  def test_existing_spool_entry_is_left_alone
    raw_dir = File.join(MEETINGS, 'F-01', 'raw')
    record_id = only_child(raw_dir)
    entry = File.join(@spool, "#{record_id}.json")
    sentinel = JSON.generate('spool_version' => 1, 'record_id' => record_id,
                             'hooked_at' => '2025-10-29T10:05:00Z', 'hook_version' => 'sentinel')
    File.write(entry, sentinel)

    status, output = run_hook(record_id, raw_dir: raw_dir)

    assert_equal 0, status.exitstatus, output
    assert_equal sentinel, File.read(entry), 'existing spool entry was changed'
    assert_equal ["#{record_id}.json"], Dir.children(@spool), 'spool dir'
    assert_writes_only_spool
  end

  # §3.3 row 1: the env file is missing.
  def test_missing_env_file_exits_1
    record_id = only_child(File.join(MEETINGS, 'F-01', 'raw'))
    status, output = run_hook_with_env_file(record_id, File.join(@sandbox, 'no-such-exporter.env'))
    assert_error_row(status, output, record_id)
  end

  # §3.3 row 1: the env file is unreadable. A directory in its place cannot be
  # read as a file, even by root (the podman container runs as root).
  def test_unreadable_env_file_exits_1
    record_id = only_child(File.join(MEETINGS, 'F-01', 'raw'))
    env_dir = File.join(@sandbox, 'exporter-env-dir')
    FileUtils.mkdir_p(env_dir)
    status, output = run_hook_with_env_file(record_id, env_dir)
    assert_error_row(status, output, record_id)
  end

  # §3.3 row 2: events.xml missing.
  def test_missing_events_xml_exits_1
    raw_dir = File.join(@sandbox, 'raw')
    record_id = '0000000000000000000000000000000000000000-1761732000000'
    FileUtils.mkdir_p(File.join(raw_dir, record_id, 'audio'))

    status, output = run_hook(record_id, raw_dir: raw_dir)

    assert_error_row(status, output, record_id)
  end

  # §3.3 row 2: events.xml unparsable.
  def test_unparsable_events_xml_exits_1
    raw_dir = File.join(@sandbox, 'raw')
    record_id = '0000000000000000000000000000000000000001-1761732000000'
    FileUtils.mkdir_p(File.join(raw_dir, record_id))
    File.write(File.join(raw_dir, record_id, 'events.xml'), "<recording meeting_id=\"x\"><meeting\n")

    status, output = run_hook(record_id, raw_dir: raw_dir)

    assert_error_row(status, output, record_id)
  end

  # §3.2 and README "Grammars": the opt-in key uses the configured prefix.
  def test_other_meta_prefix_is_not_opted_in
    raw_dir = File.join(MEETINGS, 'F-01', 'raw')
    record_id = only_child(raw_dir)

    status, output = run_hook(record_id, raw_dir: raw_dir, prefix: 'other')

    assert_equal 0, status.exitstatus, output
    assert_empty Dir.children(@spool), 'no spool entry expected'
    assert_includes output, 'not_opted_in'
  end

  # §3.1: no network, no processes, no heavy work. A static check of the whole
  # source, including strings and comments (§3.1): no network library, no
  # hashing, and none of backticks, %x, system, exec, spawn, fork, IO.popen,
  # Open3 or open("|...").
  def test_static_no_network_no_hashing_no_subprocess
    code = File.read(HOOK)
    forbidden = {
      'network library' => /\brequire(_relative)?\s*\(?\s*['"](socket|net\/\w+|open-uri|openssl|resolv|webrick)['"]/,
      'network class' => /\b(TCPSocket|UDPSocket|UNIXSocket|Socket|Net::\w+|OpenURI|URI\.open)\b/,
      'hashing' => /\brequire\s*\(?\s*['"]digest/,
      'child process' => /\b(system|exec|spawn|fork)\b|\bIO\.popen\b|\bOpen3\b|%x[{(\[<|!]|`|\bopen\s*\(?\s*['"]\|/
    }
    forbidden.each do |what, pattern|
      refute_match pattern, code, "#{HOOK_REL} uses a #{what}"
    end
  end

  private

  # §3.3 rows 1-2: exit 1, an ERROR line naming the record (§3.1: the log
  # carries record_id and decision), no Ruby backtrace, no spool entry.
  def assert_error_row(status, output, record_id)
    assert_equal 1, status.exitstatus, output
    refute_match(/\.rb:\d+:in\s/, output, 'uncaught exception instead of a logged ERROR')
    assert_logged(output, 'ERROR', record_id)
    assert_empty Dir.children(@spool), 'no spool entry expected'
  end

  # §3.1 and README "Logging": every output line is a JSON object with the
  # keys time, level and msg.
  def log_lines(output)
    output.lines.map(&:strip).reject(&:empty?).map do |line|
      obj = begin
        JSON.parse(line)
      rescue JSON::ParserError
        nil
      end
      assert_kind_of Hash, obj, "log line is not a JSON object: #{line}"
      %w[time level msg].each { |k| assert obj.key?(k), "log line without #{k}: #{line}" }
      obj
    end
  end

  # A line at level (compared case-insensitively, README "Logging") that
  # carries every one of values as a value.
  def assert_logged(output, level, *values)
    found = log_lines(output).any? do |obj|
      obj['level'].to_s.casecmp?(level) && values.all? { |v| obj.values.include?(v) }
    end
    assert found, "no #{level} line with #{values.inspect}\n#{output}"
  end

  # The hook runs with a clean environment: only the env file variable and
  # what ruby needs, so no RECAP_EXPORTER_* of the caller can leak in.
  def run_hook_with_env_file(record_id, env_file)
    env = { ENV_FILE_VAR => env_file, 'PATH' => ENV.fetch('PATH', '/usr/bin:/bin'), 'HOME' => @cwd, 'TMPDIR' => @cwd }
    out, err, status = Open3.capture3(env, RbConfig.ruby, HOOK, '-m', record_id, chdir: @cwd, unsetenv_others: true)
    [status, out + err]
  end

  def run_hook(record_id, raw_dir:, prefix: 'recap')
    env_file = File.join(@sandbox, 'exporter.env')
    File.write(env_file, <<~ENV)
      RECAP_EXPORTER_SERVICE_URL=https://127.0.0.1:1
      RECAP_EXPORTER_BBB_SERVER_ID=bbb-fixture-a
      RECAP_EXPORTER_META_PREFIX=#{prefix}
      RECAP_EXPORTER_RAW_DIR=#{raw_dir}
      RECAP_EXPORTER_SPOOL_DIR=#{@spool}
    ENV
    run_hook_with_env_file(record_id, env_file)
  end

  def assert_spool(expected, record_id, output)
    if expected.nil?
      assert_empty Dir.children(@spool), "no spool entry expected\n#{output}"
      return
    end
    name = "#{record_id}.json"
    assert_equal [name], Dir.children(@spool), "spool dir\n#{output}"
    entry = JSON.parse(File.read(File.join(@spool, name)))
    assert_equal SPOOL_KEYS, entry.keys.sort, 'spool entry fields (no metadata, §3.4)'
    expected.each { |k, v| assert_equal v, entry[k], "spool entry #{k}" }
    assert_match RFC3339_UTC, entry['hooked_at'], 'hooked_at is RFC 3339 UTC'
    Time.iso8601(entry['hooked_at'])
    assert_kind_of String, entry['hook_version']
    refute_empty entry['hook_version'], 'hook_version'
  end

  def assert_log(decision, record_id, output)
    CANARIES.each { |c| refute_includes output, c, 'hook log carries more than record_id and decision' }
    lines = log_lines(output)
    return if decision == 'spool'

    refute_empty lines, 'decision is logged'
    assert_logged(output, decision == 'no_marks' ? 'WARN' : 'INFO', decision, record_id)
  end

  # Nothing may appear outside the spool dir: the sandbox holds only the env
  # file, the spool dir and the (still empty) working directory.
  def assert_writes_only_spool
    assert_equal %w[cwd exporter.env spool], Dir.children(@sandbox).reject { |c| c == 'raw' }.sort,
                 'files written outside the spool dir'
    assert_empty Dir.children(@cwd), 'files written to the working directory'
    leftovers = Dir.children(@spool).grep(/\A\.tmp-/)
    assert_empty leftovers, 'temporary spool file left behind'
  end

  def only_child(raw_dir)
    children = Dir.children(raw_dir)
    assert_equal 1, children.size, "#{raw_dir} holds exactly one record dir"
    children.first
  end

  def tree_snapshot(dir)
    Dir.glob('**/*', File::FNM_DOTMATCH, base: dir).sort.map do |rel|
      st = File.lstat(File.join(dir, rel))
      [rel, st.ftype, st.size, st.mtime.to_r]
    end
  end
end
