// turnbeam-speech transcribes a prepared WAV for Turnbeam with Apple's on-device
// SpeechTranscriber (macOS 26 or later). Turnbeam builds it from this source on
// the user's Mac during `turnbeam setup --model apple`; nothing is uploaded.
//
//   turnbeam-speech status     --locale TAG
//   turnbeam-speech install    --locale TAG
//   turnbeam-speech transcribe --locale TAG --input WAV --output JSON
//
// TAG is a BCP-47 language such as en-US, or "auto" for the Mac's language.
// status and install print one JSON object; transcribe writes words with times
// in seconds to --output. Failures exit with a code from Code, never partial JSON.
import AVFoundation
import Foundation
import Speech

enum Code: Int32 {
    case failure = 1
    case usage = 2
    case unavailable = 3
    case unsupportedLanguage = 4
    case notInstalled = 5
}

struct Failure: Error {
    let code: Code
}

struct State: Encodable {
    let locale: String
    let status: String
    let os: String
}

struct Word: Encodable {
    let word: String
    let start: Double
    let end: Double
}

struct Transcript: Encodable {
    let locale: String
    let os: String
    let text: String
    let words: [Word]
}

@main
struct Helper {
    static func main() async {
        do {
            try await run(Array(CommandLine.arguments.dropFirst()))
            exit(0)
        } catch let failure as Failure {
            exit(failure.code.rawValue)
        } catch {
            FileHandle.standardError.write(Data("\(error)\n".utf8))
            exit(Code.failure.rawValue)
        }
    }

    static func run(_ args: [String]) async throws {
        guard let command = args.first, args.count % 2 == 1 else { throw Failure(code: .usage) }
        var options: [String: String] = [:]
        for i in stride(from: 1, to: args.count, by: 2) {
            options[args[i]] = args[i + 1]
        }
        guard SpeechTranscriber.isAvailable else { throw Failure(code: .unavailable) }
        let tag = options["--locale"] ?? "auto"
        let wanted = tag == "auto" ? Locale.current : Locale(identifier: tag)
        guard let locale = await choose(wanted) else { throw Failure(code: .unsupportedLanguage) }
        let transcriber = SpeechTranscriber(locale: locale, transcriptionOptions: [], reportingOptions: [], attributeOptions: [.audioTimeRange])
        let os = ProcessInfo.processInfo.operatingSystemVersionString
        let language = locale.identifier(.bcp47)
        switch command {
        case "status":
            try printJSON(State(locale: language, status: name(await AssetInventory.status(forModules: [transcriber])), os: os))
        case "install":
            if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                try await request.downloadAndInstall()
            }
            try printJSON(State(locale: language, status: name(await AssetInventory.status(forModules: [transcriber])), os: os))
        case "transcribe":
            guard let input = options["--input"], let output = options["--output"] else { throw Failure(code: .usage) }
            guard await AssetInventory.status(forModules: [transcriber]) == .installed else { throw Failure(code: .notInstalled) }
            let words = try await transcribe(URL(fileURLWithPath: input), with: transcriber)
            let text = words.map(\.word).joined(separator: " ")
            let data = try JSONEncoder().encode(Transcript(locale: language, os: os, text: text, words: words))
            try data.write(to: URL(fileURLWithPath: output), options: .withoutOverwriting)
        default:
            throw Failure(code: .usage)
        }
    }

    // choose picks the supported locale for a language: an exact match, else the
    // same region, else the language's main region (en-US for en, es-ES for es),
    // else the first of that language. macOS's own equivalence can pick a distant
    // variant, such as en-ZA for en-MX, so it is not used.
    static func choose(_ wanted: Locale) async -> Locale? {
        let supported = await SpeechTranscriber.supportedLocales
        let tag = wanted.identifier(.bcp47)
        if let exact = supported.first(where: { $0.identifier(.bcp47) == tag }) {
            return exact
        }
        guard let language = wanted.language.languageCode else { return nil }
        let same = supported.filter { $0.language.languageCode == language }.sorted { $0.identifier(.bcp47) < $1.identifier(.bcp47) }
        if let region = wanted.region, let match = same.first(where: { $0.region == region }) {
            return match
        }
        let main = Locale.Language(identifier: language.identifier).maximalIdentifier
        if let region = Locale.Language(identifier: main).region, let match = same.first(where: { $0.region == region }) {
            return match
        }
        return same.first
    }

    // transcribe keeps every run that carries an audio time range: one word,
    // with its punctuation, on the recording's own clock.
    static func transcribe(_ url: URL, with transcriber: SpeechTranscriber) async throws -> [Word] {
        let file = try AVAudioFile(forReading: url)
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        let collector = Task {
            var words: [Word] = []
            for try await result in transcriber.results {
                for run in result.text.runs {
                    guard let range = run.audioTimeRange else { continue }
                    let text = String(result.text[run.range].characters).trimmingCharacters(in: .whitespacesAndNewlines)
                    if !text.isEmpty {
                        words.append(Word(word: text, start: range.start.seconds, end: range.end.seconds))
                    }
                }
            }
            return words
        }
        if let last = try await analyzer.analyzeSequence(from: file) {
            try await analyzer.finalizeAndFinish(through: last)
        } else {
            await analyzer.cancelAndFinishNow()
        }
        return try await collector.value
    }

    static func name(_ status: AssetInventory.Status) -> String {
        switch status {
        case .installed: return "installed"
        case .downloading: return "downloading"
        case .supported: return "supported"
        default: return "unsupported"
        }
    }

    static func printJSON<T: Encodable>(_ value: T) throws {
        FileHandle.standardOutput.write(try JSONEncoder().encode(value) + Data("\n".utf8))
    }
}
