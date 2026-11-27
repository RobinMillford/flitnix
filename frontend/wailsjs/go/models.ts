export namespace audit {

	export class Entry {
	    id: string;
	    // Go type: time
	    timestamp: any;
	    eventType: string;
	    roomId?: string;
	    guestId?: string;
	    guestName?: string;
	    fileName?: string;
	    fileSize?: number;
	    sha256?: string;
	    outcome: string;
	    details?: string;

	    static createFrom(source: any = {}) {
	        return new Entry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.timestamp = this.convertValues(source["timestamp"], null);
	        this.eventType = source["eventType"];
	        this.roomId = source["roomId"];
	        this.guestId = source["guestId"];
	        this.guestName = source["guestName"];
	        this.fileName = source["fileName"];
	        this.fileSize = source["fileSize"];
	        this.sha256 = source["sha256"];
	        this.outcome = source["outcome"];
	        this.details = source["details"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Filter {
	    roomId?: string;
	    guestId?: string;
	    eventType?: string;
	    outcome?: string;
	    // Go type: time
	    since?: any;
	    // Go type: time
	    until?: any;
	    limit?: number;
	    offset?: number;

	    static createFrom(source: any = {}) {
	        return new Filter(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.roomId = source["roomId"];
	        this.guestId = source["guestId"];
	        this.eventType = source["eventType"];
	        this.outcome = source["outcome"];
	        this.since = this.convertValues(source["since"], null);
	        this.until = this.convertValues(source["until"], null);
	        this.limit = source["limit"];
	        this.offset = source["offset"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace client {

	export class PairResult {
	    hostId: string;
	    deviceId: string;
	    hostPublicKey: string;
	    sasDisplay: string;
	    hostName: string;

	    static createFrom(source: any = {}) {
	        return new PairResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hostId = source["hostId"];
	        this.deviceId = source["deviceId"];
	        this.hostPublicKey = source["hostPublicKey"];
	        this.sasDisplay = source["sasDisplay"];
	        this.hostName = source["hostName"];
	    }
	}

	export class PeerProbeInfo {
	    address: string;
	    hostId: string;
	    publicKey: string;
	    tlsActive: boolean;
	    reachable: boolean;

	    static createFrom(source: any = {}) {
	        return new PeerProbeInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.hostId = source["hostId"];
	        this.publicKey = source["publicKey"];
	        this.tlsActive = source["tlsActive"];
	        this.reachable = source["reachable"];
	    }
	}

}

export namespace devices {

	export class Device {
	    id: string;
	    name: string;
	    publicKey: string;
	    // Go type: time
	    pairedAt: any;
	    // Go type: time
	    lastSeen: any;
	    autoApprove: boolean;
	    maxAutoApproveBytes: number;
	    allowExecutables: boolean;
	    allowScripts: boolean;

	    static createFrom(source: any = {}) {
	        return new Device(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.publicKey = source["publicKey"];
	        this.pairedAt = this.convertValues(source["pairedAt"], null);
	        this.lastSeen = this.convertValues(source["lastSeen"], null);
	        this.autoApprove = source["autoApprove"];
	        this.maxAutoApproveBytes = source["maxAutoApproveBytes"];
	        this.allowExecutables = source["allowExecutables"];
	        this.allowScripts = source["allowScripts"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PairingStatusView {
	    locked: boolean;
	    lockoutDuration: number;
	    active: boolean;
	    requiresApproval: boolean;
	    requestId?: string;
	    deviceName?: string;
	    clientId?: string;
	    sas?: string;
	    remainingSeconds: number;
	    failureCount: number;

	    static createFrom(source: any = {}) {
	        return new PairingStatusView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.locked = source["locked"];
	        this.lockoutDuration = source["lockoutDuration"];
	        this.active = source["active"];
	        this.requiresApproval = source["requiresApproval"];
	        this.requestId = source["requestId"];
	        this.deviceName = source["deviceName"];
	        this.clientId = source["clientId"];
	        this.sas = source["sas"];
	        this.remainingSeconds = source["remainingSeconds"];
	        this.failureCount = source["failureCount"];
	    }
	}
	export class DevicePolicy {
	    autoApprove: boolean;
	    maxAutoApproveBytes: number;
	    allowExecutables: boolean;
	    allowScripts: boolean;

	    static createFrom(source: any = {}) {
	        return new DevicePolicy(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.autoApprove = source["autoApprove"];
	        this.maxAutoApproveBytes = source["maxAutoApproveBytes"];
	        this.allowExecutables = source["allowExecutables"];
	        this.allowScripts = source["allowScripts"];
	    }
	}
	export class VisibilityState {
	    mode: string;
	    previousRestrictiveMode: string;
	    // Go type: time
	    temporaryExpiresAt?: any;
	    qrSharingPaused: boolean;
	    remainingSeconds: number;
	    // Go type: time
	    updatedAt?: any;

	    static createFrom(source: any = {}) {
	        return new VisibilityState(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.previousRestrictiveMode = source["previousRestrictiveMode"];
	        this.temporaryExpiresAt = source["temporaryExpiresAt"];
	        this.qrSharingPaused = source["qrSharingPaused"];
	        this.remainingSeconds = source["remainingSeconds"];
	        this.updatedAt = source["updatedAt"];
	    }
	}

}

export namespace discovery {

	export class Peer {
	    hostId: string;
	    publicKey: string;
	    deviceName: string;
	    os: string;
	    version: string;
	    address: string;
	    port: number;
	    addresses?: string[];
	    trusted: boolean;
	    autoApprove: boolean;
	    nameConflictWarning?: boolean;
	    interfaceName?: string;
	    // Go type: time
	    lastSeen?: any;

	    static createFrom(source: any = {}) {
	        return new Peer(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hostId = source["hostId"];
	        this.publicKey = source["publicKey"];
	        this.deviceName = source["deviceName"];
	        this.os = source["os"];
	        this.version = source["version"];
	        this.address = source["address"];
	        this.port = source["port"];
	        this.addresses = source["addresses"];
	        this.trusted = source["trusted"];
	        this.autoApprove = source["autoApprove"];
	        this.nameConflictWarning = source["nameConflictWarning"];
	        this.interfaceName = source["interfaceName"];
	        this.lastSeen = source["lastSeen"];
	    }
	}

}

export namespace doctor {

	export class DiagnosticItem {
	    id: string;
	    title: string;
	    status: string;
	    message: string;
	    remediation?: string;
	    firewallState?: string;
	    affectedPort?: number;
	    command?: string;

	    static createFrom(source: any = {}) {
	        return new DiagnosticItem(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.status = source["status"];
	        this.message = source["message"];
	        this.remediation = source["remediation"];
	        this.firewallState = source["firewallState"];
	        this.affectedPort = source["affectedPort"];
	        this.command = source["command"];
	    }
	}
	export class DiagnosticReport {
	    overallStatus: string;
	    // Go type: time
	    timestamp: any;
	    summary: string;
	    items: DiagnosticItem[];

	    static createFrom(source: any = {}) {
	        return new DiagnosticReport(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.overallStatus = source["overallStatus"];
	        this.timestamp = this.convertValues(source["timestamp"], null);
	        this.summary = source["summary"];
	        this.items = this.convertValues(source["items"], DiagnosticItem);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace guest {

	export class ExpectedParticipant {
	    id: string;
	    identifier: string;
	    name: string;
	    assignedGuestId?: string;
	    status: string;
	    submissionCount: number;
	    // Go type: time
	    lastActivity?: any;

	    static createFrom(source: any = {}) {
	        return new ExpectedParticipant(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.identifier = source["identifier"];
	        this.name = source["name"];
	        this.assignedGuestId = source["assignedGuestId"];
	        this.status = source["status"];
	        this.submissionCount = source["submissionCount"];
	        this.lastActivity = this.convertValues(source["lastActivity"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ExportResult {
	    targetPath: string;
	    totalFiles: number;
	    totalBytes: number;

	    static createFrom(source: any = {}) {
	        return new ExportResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.targetPath = source["targetPath"];
	        this.totalFiles = source["totalFiles"];
	        this.totalBytes = source["totalBytes"];
	    }
	}
	export class FormField {
	    id: string;
	    label: string;
	    type: string;
	    required: boolean;
	    options?: string[];
	    maxLength?: number;

	    static createFrom(source: any = {}) {
	        return new FormField(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.label = source["label"];
	        this.type = source["type"];
	        this.required = source["required"];
	        this.options = source["options"];
	        this.maxLength = source["maxLength"];
	    }
	}
	export class Preset {
	    id: string;
	    name: string;
	    title: string;
	    description: string;
	    durationMinutes: number;
	    collisionPolicy: string;
	    formSchema?: FormField[];

	    static createFrom(source: any = {}) {
	        return new Preset(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.title = source["title"];
	        this.description = source["description"];
	        this.durationMinutes = source["durationMinutes"];
	        this.collisionPolicy = source["collisionPolicy"];
	        this.formSchema = this.convertValues(source["formSchema"], FormField);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Template {
	    id: string;
	    name: string;
	    title: string;
	    description?: string;
	    durationMinutes: number;
	    collisionPolicy: string;
	    destinationRoot?: string;
	    formSchema?: FormField[];
	    expectedParticipants?: ExpectedParticipant[];
	    version: number;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;

	    static createFrom(source: any = {}) {
	        return new Template(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.title = source["title"];
	        this.description = source["description"];
	        this.durationMinutes = source["durationMinutes"];
	        this.collisionPolicy = source["collisionPolicy"];
	        this.destinationRoot = source["destinationRoot"];
	        this.formSchema = this.convertValues(source["formSchema"], FormField);
	        this.expectedParticipants = this.convertValues(source["expectedParticipants"], ExpectedParticipant);
	        this.version = source["version"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace main {

	export class LocalPeerInfo {
	    hostId: string;
	    publicKey: string;
	    fingerprint: string;
	    deviceName: string;
	    os: string;
	    version: string;
	    address: string;
	    port: number;

	    static createFrom(source: any = {}) {
	        return new LocalPeerInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hostId = source["hostId"];
	        this.publicKey = source["publicKey"];
	        this.fingerprint = source["fingerprint"];
	        this.deviceName = source["deviceName"];
	        this.os = source["os"];
	        this.version = source["version"];
	        this.address = source["address"];
	        this.port = source["port"];
	    }
	}

	export class PeerPairRequest {
	    address: string;
	    port?: number;
	    expectedHostId?: string;
	    expectedPubKey?: string;
	    peerName?: string;
	    shortCode?: string;
	    pairingToken?: string;

	    static createFrom(source: any = {}) {
	        return new PeerPairRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.port = source["port"];
	        this.expectedHostId = source["expectedHostId"];
	        this.expectedPubKey = source["expectedPubKey"];
	        this.peerName = source["peerName"];
	        this.shortCode = source["shortCode"];
	        this.pairingToken = source["pairingToken"];
	    }
	}

	export class PeerPairSASPrompt {
	    requestId: string;
	    peerName: string;
	    peerHostId: string;
	    sas: string;
	    isPinned: boolean;

	    static createFrom(source: any = {}) {
	        return new PeerPairSASPrompt(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.requestId = source["requestId"];
	        this.peerName = source["peerName"];
	        this.peerHostId = source["peerHostId"];
	        this.sas = source["sas"];
	        this.isPinned = source["isPinned"];
	    }
	}

	export class PeerSendRequest {
	    address: string;
	    port?: number;
	    expectedHostId?: string;
	    expectedPubKey?: string;
	    peerName?: string;
	    shortCode?: string;
	    pairingToken?: string;
	    paths: string[];

	    static createFrom(source: any = {}) {
	        return new PeerSendRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.port = source["port"];
	        this.expectedHostId = source["expectedHostId"];
	        this.expectedPubKey = source["expectedPubKey"];
	        this.peerName = source["peerName"];
	        this.shortCode = source["shortCode"];
	        this.pairingToken = source["pairingToken"];
	        this.paths = source["paths"];
	    }
	}

	export class PeerTransferJob {
	    id: string;
	    peerHostId?: string;
	    peerName: string;
	    peerAddress: string;
	    kind: string;
	    rootName?: string;
	    totalItems: number;
	    completedItems: number;
	    totalBytes: number;
	    sentBytes: number;
	    speed: number;
	    state: string;
	    currentFile?: string;
	    trustedPeer: boolean;
	    verified: boolean;
	    error?: string;
	    // Go type: time
	    createdAt?: any;
	    // Go type: time
	    completedAt?: any;

	    static createFrom(source: any = {}) {
	        return new PeerTransferJob(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.peerHostId = source["peerHostId"];
	        this.peerName = source["peerName"];
	        this.peerAddress = source["peerAddress"];
	        this.kind = source["kind"];
	        this.rootName = source["rootName"];
	        this.totalItems = source["totalItems"];
	        this.completedItems = source["completedItems"];
	        this.totalBytes = source["totalBytes"];
	        this.sentBytes = source["sentBytes"];
	        this.speed = source["speed"];
	        this.state = source["state"];
	        this.currentFile = source["currentFile"];
	        this.trustedPeer = source["trustedPeer"];
	        this.verified = source["verified"];
	        this.error = source["error"];
	        this.createdAt = source["createdAt"];
	        this.completedAt = source["completedAt"];
	    }
	}

	export class AuditListResult {
	    entries: audit.Entry[];
	    total: number;

	    static createFrom(source: any = {}) {
	        return new AuditListResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.entries = this.convertValues(source["entries"], audit.Entry);
	        this.total = source["total"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class BatchItemView {
	    id: string;
	    batchId: string;
	    sequence: number;
	    relativePath?: string;
	    displayName: string;
	    declaredSize: number;
	    transferred: number;
	    state: string;
	    resumable?: boolean;
	    sha256?: string;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new BatchItemView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.batchId = source["batchId"];
	        this.sequence = source["sequence"];
	        this.relativePath = source["relativePath"];
	        this.displayName = source["displayName"];
	        this.declaredSize = source["declaredSize"];
	        this.transferred = source["transferred"];
	        this.state = source["state"];
	        this.resumable = source["resumable"];
	        this.sha256 = source["sha256"];
	        this.error = source["error"];
	    }
	}
	export class BatchView {
	    id: string;
	    direction: string;
	    kind: string;
	    rootName?: string;
	    items: BatchItemView[];
	    totalItemCount: number;
	    declaredTotalBytes: number;
	    transferredBytes: number;
	    state: string;
	    approvalState: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    startedAt?: any;
	    // Go type: time
	    completedAt?: any;
	    error?: string;
	    storageFits: boolean;
	    storageWarning?: string;
	    deviceId?: string;
	    deviceName?: string;
	    trustedDevice?: boolean;
	    approvalReason?: string;

	    static createFrom(source: any = {}) {
	        return new BatchView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.direction = source["direction"];
	        this.kind = source["kind"];
	        this.rootName = source["rootName"];
	        this.items = this.convertValues(source["items"], BatchItemView);
	        this.totalItemCount = source["totalItemCount"];
	        this.declaredTotalBytes = source["declaredTotalBytes"];
	        this.transferredBytes = source["transferredBytes"];
	        this.state = source["state"];
	        this.approvalState = source["approvalState"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.completedAt = this.convertValues(source["completedAt"], null);
	        this.error = source["error"];
	        this.storageFits = source["storageFits"];
	        this.storageWarning = source["storageWarning"];
	        this.deviceId = source["deviceId"];
	        this.deviceName = source["deviceName"];
	        this.trustedDevice = source["trustedDevice"];
	        this.approvalReason = source["approvalReason"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DownloadView {
	    id: string;
	    fileId: string;
	    name: string;
	    kind: string;
	    size: number;
	    // Go type: time
	    created: any;
	    // Go type: time
	    started: any;
	    // Go type: time
	    finished?: any;
	    state: string;
	    written: number;
	    speed: number;
	    sha256?: string;
	    reason?: string;

	    static createFrom(source: any = {}) {
	        return new DownloadView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.fileId = source["fileId"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.size = source["size"];
	        this.created = this.convertValues(source["created"], null);
	        this.started = this.convertValues(source["started"], null);
	        this.finished = this.convertValues(source["finished"], null);
	        this.state = source["state"];
	        this.written = source["written"];
	        this.speed = source["speed"];
	        this.sha256 = source["sha256"];
	        this.reason = source["reason"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GuestRoomConfigView {
	    destinationDir: string;
	    durationMinutes: number;
	    title: string;
	    description?: string;
	    deadlineRFC3339?: string;
	    collisionPolicy: string;
	    formSchema?: guest.FormField[];
	    expectedParticipants?: guest.ExpectedParticipant[];

	    static createFrom(source: any = {}) {
	        return new GuestRoomConfigView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.destinationDir = source["destinationDir"];
	        this.durationMinutes = source["durationMinutes"];
	        this.title = source["title"];
	        this.description = source["description"];
	        this.deadlineRFC3339 = source["deadlineRFC3339"];
	        this.collisionPolicy = source["collisionPolicy"];
	        this.formSchema = this.convertValues(source["formSchema"], guest.FormField);
	        this.expectedParticipants = this.convertValues(source["expectedParticipants"], guest.ExpectedParticipant);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GuestUploadView {
	    id: string;
	    guestId: string;
	    guestName: string;
	    fileName: string;
	    size: number;
	    state: string;
	    transferred: number;
	    speed: number;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    completedAt?: any;
	    sha256?: string;
	    error?: string;
	    formValues?: Record<string, string>;

	    static createFrom(source: any = {}) {
	        return new GuestUploadView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.guestId = source["guestId"];
	        this.guestName = source["guestName"];
	        this.fileName = source["fileName"];
	        this.size = source["size"];
	        this.state = source["state"];
	        this.transferred = source["transferred"];
	        this.speed = source["speed"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.completedAt = this.convertValues(source["completedAt"], null);
	        this.sha256 = source["sha256"];
	        this.error = source["error"];
	        this.formValues = source["formValues"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GuestView {
	    id: string;
	    nickname: string;
	    participantId?: string;
	    state: string;
	    // Go type: time
	    createdAt: any;

	    static createFrom(source: any = {}) {
	        return new GuestView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.nickname = source["nickname"];
	        this.participantId = source["participantId"];
	        this.state = source["state"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GuestRoomView {
	    id: string;
	    date: string;
	    destinationRoot: string;
	    roomFolder: string;
	    title: string;
	    description?: string;
	    // Go type: time
	    deadline?: any;
	    collisionPolicy: string;
	    state: string;
	    durationSeconds: number;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    expiresAt: any;
	    inviteSecret?: string;
	    inviteUrl?: string;
	    qrDataUrl?: string;
	    formSchema?: guest.FormField[];
	    roster?: guest.ExpectedParticipant[];
	    hasRoster: boolean;

	    static createFrom(source: any = {}) {
	        return new GuestRoomView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.date = source["date"];
	        this.destinationRoot = source["destinationRoot"];
	        this.roomFolder = source["roomFolder"];
	        this.title = source["title"];
	        this.description = source["description"];
	        this.deadline = this.convertValues(source["deadline"], null);
	        this.collisionPolicy = source["collisionPolicy"];
	        this.state = source["state"];
	        this.durationSeconds = source["durationSeconds"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.expiresAt = this.convertValues(source["expiresAt"], null);
	        this.inviteSecret = source["inviteSecret"];
	        this.inviteUrl = source["inviteUrl"];
	        this.qrDataUrl = source["qrDataUrl"];
	        this.formSchema = this.convertValues(source["formSchema"], guest.FormField);
	        this.roster = this.convertValues(source["roster"], guest.ExpectedParticipant);
	        this.hasRoster = source["hasRoster"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GuestRoomStateView {
	    room?: GuestRoomView;
	    pendingGuests: GuestView[];
	    approvedGuests: GuestView[];
	    pendingUploads: GuestUploadView[];
	    completedUploads: GuestUploadView[];
	    recentUploads: GuestUploadView[];
	    roster?: guest.ExpectedParticipant[];

	    static createFrom(source: any = {}) {
	        return new GuestRoomStateView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.room = this.convertValues(source["room"], GuestRoomView);
	        this.pendingGuests = this.convertValues(source["pendingGuests"], GuestView);
	        this.approvedGuests = this.convertValues(source["approvedGuests"], GuestView);
	        this.pendingUploads = this.convertValues(source["pendingUploads"], GuestUploadView);
	        this.completedUploads = this.convertValues(source["completedUploads"], GuestUploadView);
	        this.recentUploads = this.convertValues(source["recentUploads"], GuestUploadView);
	        this.roster = this.convertValues(source["roster"], guest.ExpectedParticipant);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}



	export class SessionView {
	    key: string;
	    // Go type: time
	    created: any;
	    // Go type: time
	    expiresAt: any;
	    pinnedTransfers: number;
	    token?: string;
	    interfaceName?: string;
	    address?: string;

	    static createFrom(source: any = {}) {
	        return new SessionView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.created = this.convertValues(source["created"], null);
	        this.expiresAt = this.convertValues(source["expiresAt"], null);
	        this.pinnedTransfers = source["pinnedTransfers"];
	        this.token = source["token"];
	        this.interfaceName = source["interfaceName"];
	        this.address = source["address"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SettingsView {
	    version: number;
	    theme: string;
	    locale: string;
	    downloadDir: string;
	    effectiveDir: string;
	    autoStartSession: boolean;
	    preserveModTimes: boolean;
	    preferredInterface: string;
	    visibilityMode?: string;
	    visibilityPreviousMode?: string;
	    visibilityExpiresAt?: string;
	    qrSharingPaused?: boolean;
	    minimizeToTray: boolean;
	    launchOnLogin: boolean;
	    traySupported?: boolean;
	    autostartSupported?: boolean;
	    notificationsEnabled: boolean;
	    quietMode: boolean;
	    hideNotificationFilenames: boolean;
	    notifyIncomingTransfers: boolean;
	    notifyCompletedTransfers: boolean;
	    notifyFailedTransfers: boolean;
	    notifyQuickText: boolean;
	    notifyPairingRequests: boolean;
	    notifyGuestRequests: boolean;
	    notificationsSupported?: boolean;

	    static createFrom(source: any = {}) {
	        return new SettingsView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.theme = source["theme"];
	        this.locale = source["locale"];
	        this.downloadDir = source["downloadDir"];
	        this.effectiveDir = source["effectiveDir"];
	        this.autoStartSession = source["autoStartSession"];
	        this.preserveModTimes = source["preserveModTimes"] !== undefined ? source["preserveModTimes"] : true;
	        this.preferredInterface = source["preferredInterface"];
	        this.visibilityMode = source["visibilityMode"];
	        this.visibilityPreviousMode = source["visibilityPreviousMode"];
	        this.visibilityExpiresAt = source["visibilityExpiresAt"];
	        this.qrSharingPaused = source["qrSharingPaused"];
	        this.minimizeToTray = source["minimizeToTray"] !== undefined ? source["minimizeToTray"] : true;
	        this.launchOnLogin = Boolean(source["launchOnLogin"]);
	        this.traySupported = Boolean(source["traySupported"]);
	        this.autostartSupported = Boolean(source["autostartSupported"]);
	        this.notificationsEnabled = source["notificationsEnabled"] !== undefined ? source["notificationsEnabled"] : true;
	        this.quietMode = Boolean(source["quietMode"]);
	        this.hideNotificationFilenames = Boolean(source["hideNotificationFilenames"]);
	        this.notifyIncomingTransfers = source["notifyIncomingTransfers"] !== undefined ? source["notifyIncomingTransfers"] : true;
	        this.notifyCompletedTransfers = source["notifyCompletedTransfers"] !== undefined ? source["notifyCompletedTransfers"] : true;
	        this.notifyFailedTransfers = source["notifyFailedTransfers"] !== undefined ? source["notifyFailedTransfers"] : true;
	        this.notifyQuickText = source["notifyQuickText"] !== undefined ? source["notifyQuickText"] : true;
	        this.notifyPairingRequests = source["notifyPairingRequests"] !== undefined ? source["notifyPairingRequests"] : true;
	        this.notifyGuestRequests = source["notifyGuestRequests"] !== undefined ? source["notifyGuestRequests"] : true;
	        this.notificationsSupported = Boolean(source["notificationsSupported"]);
	    }
	}
	export class SharedFileView {
	    id: string;
	    name: string;
	    size: number;
	    // Go type: time
	    created: any;

	    static createFrom(source: any = {}) {
	        return new SharedFileView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.created = this.convertValues(source["created"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SharedSourceView {
	    id: string;
	    kind: string;
	    name: string;
	    rootName?: string;
	    itemCount: number;
	    totalBytes: number;
	    // Go type: time
	    created: any;

	    static createFrom(source: any = {}) {
	        return new SharedSourceView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.rootName = source["rootName"];
	        this.itemCount = source["itemCount"];
	        this.totalBytes = source["totalBytes"];
	        this.created = this.convertValues(source["created"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TextMessageView {
	    id: string;
	    sender: string;
	    content: string;
	    isLink: boolean;
	    url?: string;
	    // Go type: time
	    createdAt: any;

	    static createFrom(source: any = {}) {
	        return new TextMessageView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.sender = source["sender"];
	        this.content = source["content"];
	        this.isLink = source["isLink"];
	        this.url = source["url"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class UploadView {
	    id: string;
	    name: string;
	    size: number;
	    // Go type: time
	    created: any;
	    state: string;
	    uploaded: number;
	    speed: number;
	    sha256?: string;
	    reason?: string;
	    resumable?: boolean;
	    committedOffset?: number;
	    storageFits: boolean;
	    storageWarning?: string;
	    deviceId?: string;
	    deviceName?: string;
	    trustedDevice?: boolean;
	    approvalReason?: string;

	    static createFrom(source: any = {}) {
	        return new UploadView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.created = this.convertValues(source["created"], null);
	        this.state = source["state"];
	        this.uploaded = source["uploaded"];
	        this.speed = source["speed"];
	        this.sha256 = source["sha256"];
	        this.reason = source["reason"];
	        this.resumable = source["resumable"];
	        this.committedOffset = source["committedOffset"];
	        this.storageFits = source["storageFits"];
	        this.storageWarning = source["storageWarning"];
	        this.deviceId = source["deviceId"];
	        this.deviceName = source["deviceName"];
	        this.trustedDevice = source["trustedDevice"];
	        this.approvalReason = source["approvalReason"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class HistoryRecordView {
	    id: string;
	    timestamp: any;
	    direction: string;
	    kind?: string;
	    displayName: string;
	    itemCount: number;
	    size: number;
	    transferred: number;
	    status: string;
	    deviceId?: string;
	    deviceName?: string;
	    sha256?: string;
	    error?: string;
	    batchId?: string;
	    canOpen: boolean;
	    canReveal: boolean;
	    canReshare: boolean;

	    static createFrom(source: any = {}) {
	        return new HistoryRecordView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"] ?? '';
	        this.timestamp = source["timestamp"];
	        this.direction = source["direction"] ?? '';
	        this.kind = source["kind"];
	        this.displayName = source["displayName"] ?? '';
	        this.itemCount = source["itemCount"] ?? 1;
	        this.size = source["size"] ?? 0;
	        this.transferred = source["transferred"] ?? 0;
	        this.status = source["status"] ?? '';
	        this.deviceId = source["deviceId"];
	        this.deviceName = source["deviceName"];
	        this.sha256 = source["sha256"];
	        this.error = source["error"];
	        this.batchId = source["batchId"];
	        this.canOpen = Boolean(source["canOpen"]);
	        this.canReveal = Boolean(source["canReveal"]);
	        this.canReshare = Boolean(source["canReshare"]);
	    }
	}

	export class HistoryFilterView {
	    query: string;
	    direction: string;
	    status: string;
	    device: string;
	    dateRange: string;
	    limit: number;
	    offset: number;

	    static createFrom(source: any = {}) {
	        return new HistoryFilterView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.query = source["query"] ?? '';
	        this.direction = source["direction"] ?? '';
	        this.status = source["status"] ?? '';
	        this.device = source["device"] ?? '';
	        this.dateRange = source["dateRange"] ?? '';
	        this.limit = source["limit"] ?? 25;
	        this.offset = source["offset"] ?? 0;
	    }
	}

	export class HistoryQueryResultView {
	    records: HistoryRecordView[];
	    total: number;
	    stats: history.Stats;
	    storeSizeBytes: number;

	    static createFrom(source: any = {}) {
	        return new HistoryQueryResultView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.records = (source["records"] ?? []).map((r: any) => new HistoryRecordView(r));
	        this.total = source["total"] ?? 0;
	        this.stats = new history.Stats(source["stats"] ?? {});
	        this.storeSizeBytes = source["storeSizeBytes"] ?? 0;
	    }
	}

}

export namespace history {

	export class Stats {
	    totalRecords: number;
	    completedCount: number;
	    failedCount: number;
	    canceledCount: number;
	    totalBytesTransferred: number;
	    incomingBytes: number;
	    outgoingBytes: number;

	    static createFrom(source: any = {}) {
	        return new Stats(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.totalRecords = source["totalRecords"] ?? 0;
	        this.completedCount = source["completedCount"] ?? 0;
	        this.failedCount = source["failedCount"] ?? 0;
	        this.canceledCount = source["canceledCount"] ?? 0;
	        this.totalBytesTransferred = source["totalBytesTransferred"] ?? 0;
	        this.incomingBytes = source["incomingBytes"] ?? 0;
	        this.outgoingBytes = source["outgoingBytes"] ?? 0;
	    }
	}

}

export namespace netenv {

	export class View {
	    name: string;
	    ip: string;
	    cidr: string;
	    family: string;
	    scope?: string;
	    suitableForQR: boolean;
	    warning?: string;
	    friendlyName: string;
	    type: string;
	    metadataAvailable: boolean;
	    connectionName: string;
	    friendlyNameSource: string;
	    active: boolean;

	    static createFrom(source: any = {}) {
	        return new View(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.ip = source["ip"];
	        this.cidr = source["cidr"];
	        this.family = source["family"];
	        this.scope = source["scope"];
	        this.suitableForQR = source["suitableForQR"];
	        this.warning = source["warning"];
	        this.friendlyName = source["friendlyName"];
	        this.type = source["type"];
	        this.metadataAvailable = source["metadataAvailable"];
	        this.connectionName = source["connectionName"];
	        this.friendlyNameSource = source["friendlyNameSource"];
	        this.active = source["active"];
	    }
	}
	export class NetworkState {
	    status: string;
	    interfaceName: string;
	    friendlyName: string;
	    type: string;
	    ssid?: string;
	    ip: string;
	    family: string;
	    cidr: string;
	    selectedInterface: string;
	    preferredInterface?: string;
	    serverAddress?: string;
	    advertisedURL?: string;
	    lastDetected: number;
	    interfaces: View[];
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new NetworkState(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.status = source["status"];
	        this.interfaceName = source["interfaceName"];
	        this.friendlyName = source["friendlyName"];
	        this.type = source["type"];
	        this.ssid = source["ssid"];
	        this.ip = source["ip"];
	        this.family = source["family"];
	        this.cidr = source["cidr"];
	        this.selectedInterface = source["selectedInterface"];
	        this.preferredInterface = source["preferredInterface"];
	        this.serverAddress = source["serverAddress"];
	        this.advertisedURL = source["advertisedURL"];
	        this.lastDetected = source["lastDetected"];
	        this.interfaces = this.convertValues(source["interfaces"], View);
	        this.error = source["error"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace power {

	export class Status {
	    supported: boolean;
	    active: boolean;
	    holders: number;
	    reason?: string;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.supported = source["supported"];
	        this.active = source["active"];
	        this.holders = source["holders"];
	        this.reason = source["reason"];
	        this.error = source["error"];
	    }
	}

}

export namespace transfer {

	export class StorageStatus {
	    targetDir: string;
	    probeDir: string;
	    targetExists: boolean;
	    available: boolean;
	    status: string;
	    totalBytes: number;
	    freeBytes: number;
	    usedBytes: number;
	    reservedBytes: number;
	    headroomBytes: number;
	    safetyMargin: number;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new StorageStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.targetDir = source["targetDir"];
	        this.probeDir = source["probeDir"];
	        this.targetExists = source["targetExists"];
	        this.available = source["available"];
	        this.status = source["status"];
	        this.totalBytes = source["totalBytes"];
	        this.freeBytes = source["freeBytes"];
	        this.usedBytes = source["usedBytes"];
	        this.reservedBytes = source["reservedBytes"];
	        this.headroomBytes = source["headroomBytes"];
	        this.safetyMargin = source["safetyMargin"];
	        this.error = source["error"];
	    }
	}

}

export namespace update {

	export class ReleaseAsset {
	    name: string;
	    downloadUrl: string;
	    size: number;
	    contentType: string;

	    static createFrom(source: any = {}) {
	        return new ReleaseAsset(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.downloadUrl = source["downloadUrl"];
	        this.size = source["size"];
	        this.contentType = source["contentType"];
	    }
	}
	export class ReleaseInfo {
	    tag: string;
	    version: string;
	    name: string;
	    releaseNotes: string;
	    // Go type: time
	    publishedAt: any;
	    htmlUrl: string;
	    prerelease: boolean;
	    asset: ReleaseAsset;
	    checksumUrl: string;
	    signatureUrl: string;

	    static createFrom(source: any = {}) {
	        return new ReleaseInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.tag = source["tag"];
	        this.version = source["version"];
	        this.name = source["name"];
	        this.releaseNotes = source["releaseNotes"];
	        this.publishedAt = this.convertValues(source["publishedAt"], null);
	        this.htmlUrl = source["htmlUrl"];
	        this.prerelease = source["prerelease"];
	        this.asset = this.convertValues(source["asset"], ReleaseAsset);
	        this.checksumUrl = source["checksumUrl"];
	        this.signatureUrl = source["signatureUrl"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CheckResult {
	    currentVersion: string;
	    latestVersion: string;
	    updateAvailable: boolean;
	    release?: ReleaseInfo;
	    installType: string;
	    // Go type: time
	    checkedAt: any;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new CheckResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.currentVersion = source["currentVersion"];
	        this.latestVersion = source["latestVersion"];
	        this.updateAvailable = source["updateAvailable"];
	        this.release = this.convertValues(source["release"], ReleaseInfo);
	        this.installType = source["installType"];
	        this.checkedAt = this.convertValues(source["checkedAt"], null);
	        this.error = source["error"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Progress {
	    status: string;
	    downloadedBytes: number;
	    totalBytes: number;
	    percentage: number;
	    speedBytesPerSec: number;
	    stagedFilePath?: string;
	    targetInstallPath?: string;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new Progress(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.status = source["status"];
	        this.downloadedBytes = source["downloadedBytes"];
	        this.totalBytes = source["totalBytes"];
	        this.percentage = source["percentage"];
	        this.speedBytesPerSec = source["speedBytesPerSec"];
	        this.stagedFilePath = source["stagedFilePath"];
	        this.targetInstallPath = source["targetInstallPath"];
	        this.error = source["error"];
	    }
	}


}

export namespace version {

	export class Info {
	    version: string;
	    prerelease?: string;
	    gitCommit?: string;
	    buildDate?: string;
	    platform: string;
	    architecture: string;
	    fullString: string;
	    isDev: boolean;

	    static createFrom(source: any = {}) {
	        return new Info(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.prerelease = source["prerelease"];
	        this.gitCommit = source["gitCommit"];
	        this.buildDate = source["buildDate"];
	        this.platform = source["platform"];
	        this.architecture = source["architecture"];
	        this.fullString = source["fullString"];
	        this.isDev = source["isDev"];
	    }
	}

}

export namespace session {

	export class ShortCodeView {
	    code: string;
	    formattedCode: string;
	    sessionKey: string;
	    // Go type: time
	    createdAt?: any;
	    // Go type: time
	    expiresAt?: any;
	    remainingSeconds: number;
	    active: boolean;
	    used: boolean;
	    expired: boolean;

	    static createFrom(source: any = {}) {
	        return new ShortCodeView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.code = source["code"];
	        this.formattedCode = source["formattedCode"];
	        this.sessionKey = source["sessionKey"];
	        this.createdAt = source["createdAt"];
	        this.expiresAt = source["expiresAt"];
	        this.remainingSeconds = source["remainingSeconds"];
	        this.active = source["active"];
	        this.used = source["used"];
	        this.expired = source["expired"];
	    }
	}

}
