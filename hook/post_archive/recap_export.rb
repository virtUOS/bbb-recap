# frozen_string_literal: true

# post_archive hook of recap-exporter (docs/reference/bbb-exporter.md §3).
#
# BBB runs it as "ruby recap_export.rb -m <record_id>" after the archive step.
# It reads the exporter env file and events.xml, decides whether the meeting
# opted in, and writes at most one small spool entry for the recap-exporter
# daemon. It never touches raw/, never uses the network and starts no other
# processes. All errors are logged as one JSON line; it never raises into BBB.

require 'json'
require 'time'
require 'rexml/document'
require 'rexml/streamlistener'

module RecapExport
  # The recap-exporter package version. The .deb packaging replaces this value
  # with the package version when it installs the hook.
  HOOK_VERSION = '0.0.0-dev'
  SPOOL_VERSION = 1

  DEFAULT_ENV_FILE = '/etc/recap-exporter/exporter.env'
  DEFAULTS = {
    'RECAP_EXPORTER_RAW_DIR' => '/var/bigbluebutton/recording/raw',
    'RECAP_EXPORTER_SPOOL_DIR' => '/var/lib/recap-exporter/spool',
    'RECAP_EXPORTER_META_PREFIX' => 'recap'
  }.freeze

  RECORD_ID = /\A[A-Za-z0-9][A-Za-z0-9_-]{0,254}\z/

  # A known failure: logged as one ERROR line with its decision, exit 1.
  class Failure < StandardError
    attr_reader :decision

    def initialize(decision, msg)
      super(msg)
      @decision = decision
    end
  end

  # Collects the three facts the hook needs from events.xml (§3.2) while
  # streaming through the file, so large files are never held in memory.
  class EventsListener
    include REXML::StreamListener

    attr_reader :enabled, :breakout, :marks

    def initialize(enabled_key)
      @enabled_key = enabled_key
      @depth = 0
      @enabled = nil
      @breakout = nil
      @marks = false
    end

    def tag_start(name, attrs)
      @depth += 1
      return unless @depth == 2 # direct children of <recording>

      case name
      when 'metadata' then @enabled = attrs[@enabled_key]
      when 'meeting' then @breakout = attrs['breakout']
      when 'event' then @marks ||= attrs['eventname'] == 'RecordStatusEvent'
      end
    end

    def tag_end(_name)
      @depth -= 1
    end
  end

  module_function

  def main(argv)
    record_id = record_id_from(argv)
    config = read_config(ENV.fetch('RECAP_EXPORTER_ENV_FILE', DEFAULT_ENV_FILE))
    events = read_events(File.join(config['RECAP_EXPORTER_RAW_DIR'], record_id, 'events.xml'),
                         "#{config['RECAP_EXPORTER_META_PREFIX']}-enabled")
    decide(record_id, events, config['RECAP_EXPORTER_SPOOL_DIR'])
  rescue Failure => e
    log('ERROR', e.message, record_id, e.decision)
    1
  rescue StandardError => e
    log('ERROR', "unexpected error: #{e.class}", record_id, 'error')
    1
  end

  def record_id_from(argv)
    i = argv.index('-m')
    value = i && argv[i + 1]
    raise Failure.new('invalid_record_id', 'missing or invalid -m record_id') unless value&.match?(RECORD_ID)

    value
  end

  # File grammar of §8.1: KEY=value per line, line trimmed, blank and '#'
  # lines skipped, split at the first '=', no spaces around it, no quote
  # processing, empty value means the default. Invalid lines and keys the hook
  # does not use are ignored; validating them is the daemon's job.
  def read_config(path)
    text = begin
      File.read(path)
    rescue SystemCallError, IOError => e
      raise Failure.new('config_unreadable', "env file missing or unreadable: #{e.class}")
    end
    config = DEFAULTS.dup
    text.each_line do |line|
      line = line.strip
      next if line.empty? || line.start_with?('#')

      key, value = line.split('=', 2)
      next unless value && DEFAULTS.key?(key) && value == value.lstrip # also rejects 'KEY = v'

      config[key] = value unless value.empty?
    end
    config
  end

  def read_events(path, enabled_key)
    listener = EventsListener.new(enabled_key)
    File.open(path) { |f| REXML::Document.parse_stream(f, listener) }
    listener
  rescue SystemCallError, IOError => e
    raise Failure.new('events_unreadable', "events.xml missing or unreadable: #{e.class}")
  rescue RuntimeError # REXML::ParseException, and the bare RuntimeError of the entity-expansion limit
    raise Failure.new('events_unparsable', 'events.xml unparsable')
  end

  # Decision table §3.3, rows 3 to 6.
  def decide(record_id, events, spool_dir)
    if events.enabled != 'true'
      log('INFO', 'meeting did not opt in', record_id, 'not_opted_in')
    elsif events.breakout == 'true'
      log('INFO', 'breakout meeting skipped', record_id, 'breakout_skipped')
    elsif !events.marks
      log('WARN', 'no recording marks in events.xml', record_id, 'no_marks')
    else
      write_spool_entry(record_id, spool_dir)
    end
    0
  end

  # §3.4: temporary file, fsync, rename. An existing entry is left alone.
  def write_spool_entry(record_id, spool_dir)
    entry = File.join(spool_dir, "#{record_id}.json")
    if File.exist?(entry)
      log('INFO', 'spool entry exists, left alone', record_id, 'spool_exists')
      return
    end

    tmp = File.join(spool_dir, ".tmp-#{record_id}-#{Process.pid}")
    begin
      File.open(tmp, File::WRONLY | File::CREAT | File::EXCL, 0o640) do |f|
        f.write(JSON.generate(spool_content(record_id)))
        f.fsync
      end
      File.rename(tmp, entry)
    ensure
      # Removes our tmp on failure, and also a stale tmp a crashed run with the same pid left behind.
      File.unlink(tmp) if File.exist?(tmp)
    end
    log('INFO', 'spool entry written', record_id, 'spool')
  end

  def spool_content(record_id)
    {
      'spool_version' => SPOOL_VERSION,
      'record_id' => record_id,
      'hooked_at' => Time.now.utc.iso8601(3),
      'hook_version' => HOOK_VERSION
    }
  end

  # One JSON line with the README "Logging" keys plus record_id and decision.
  # Nothing else: no meta values, meeting IDs, names or paths.
  def log(level, msg, record_id, decision)
    line = { 'time' => Time.now.utc.iso8601(3), 'level' => level, 'msg' => msg }
    line['record_id'] = record_id if record_id
    line['decision'] = decision
    $stdout.puts(JSON.generate(line))
    $stdout.flush
  rescue StandardError
    nil # logging must never fail the hook
  end
end

exit(RecapExport.main(ARGV))
