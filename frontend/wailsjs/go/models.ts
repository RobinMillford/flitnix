export namespace doctor {

	export class DiagnosticItem {
	    id: string;
	    title: string;
	    status: string;
	    message: string;
	    remediation?: string;

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

export namespace main {

	export class BatchItemView {
	    id: string;
	    batchId: string;
	    sequence: number;
	    relativePath?: string;
	    displayName: string;
	    declaredSize: number;
	    transferred: number;
	    state: string;
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
	    state: string;
	    durationSeconds: number;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    expiresAt: any;
	    inviteSecret?: string;
	    inviteUrl?: string;
	    qrDataUrl?: string;

	    static createFrom(source: any = {}) {
	        return new GuestRoomView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.date = source["date"];
	        this.destinationRoot = source["destinationRoot"];
	        this.roomFolder = source["roomFolder"];
	        this.state = source["state"];
	        this.durationSeconds = source["durationSeconds"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.expiresAt = this.convertValues(source["expiresAt"], null);
	        this.inviteSecret = source["inviteSecret"];
	        this.inviteUrl = source["inviteUrl"];
	        this.qrDataUrl = source["qrDataUrl"];
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
	    reason?: string;

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

}

export namespace netenv {

	export class View {
	    name: string;
	    ip: string;
	    cidr: string;
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
	        this.friendlyName = source["friendlyName"];
	        this.type = source["type"];
	        this.metadataAvailable = source["metadataAvailable"];
	        this.connectionName = source["connectionName"];
	        this.friendlyNameSource = source["friendlyNameSource"];
	        this.active = source["active"];
	    }
	}

}

