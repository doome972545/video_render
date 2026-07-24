export namespace app {
	
	export class StatusUpdate {
	    jobId: string;
	    phase: string;
	    total: number;
	    completed: number;
	    failed: number;
	    pending: number;
	    running: number;
	    cancelled: number;
	    percent: number;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new StatusUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.jobId = source["jobId"];
	        this.phase = source["phase"];
	        this.total = source["total"];
	        this.completed = source["completed"];
	        this.failed = source["failed"];
	        this.pending = source["pending"];
	        this.running = source["running"];
	        this.cancelled = source["cancelled"];
	        this.percent = source["percent"];
	        this.error = source["error"];
	    }
	}

}

export namespace main {
	
	export class StartJobOptions {
	    source: string;
	    variants: number;
	    seed: number;
	    outputDir: string;
	    musicPath: string;
	    musicVolume: number;
	    subtitle: string;
	    subtitlePos: string;
	    extraEffects: boolean;
	    flip: string;
	
	    static createFrom(source: any = {}) {
	        return new StartJobOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.source = source["source"];
	        this.variants = source["variants"];
	        this.seed = source["seed"];
	        this.outputDir = source["outputDir"];
	        this.musicPath = source["musicPath"];
	        this.musicVolume = source["musicVolume"];
	        this.subtitle = source["subtitle"];
	        this.subtitlePos = source["subtitlePos"];
	        this.extraEffects = source["extraEffects"];
	        this.flip = source["flip"];
	    }
	}

}

